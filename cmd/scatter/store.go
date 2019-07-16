// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/eliasnaur/libsignal-protocol-go/ecc"
	"github.com/eliasnaur/libsignal-protocol-go/keys/identity"
	"github.com/eliasnaur/libsignal-protocol-go/keys/prekey"
	"github.com/eliasnaur/libsignal-protocol-go/protocol"
	"github.com/eliasnaur/libsignal-protocol-go/util/optional"
)

type Store struct {
	db *bolt.DB
}

type Account struct {
	User     string
	Password string
	IMAPHost string
	SMTPHost string
}

type IMAPState struct {
	Mailbox     string
	LastUID     uint32
	UIDValidity uint32
}

type Message struct {
	ID      uint64
	Thread  string
	Own     bool
	Sent    bool
	Time    time.Time
	Message string
}

type WireMessage struct {
	Type       MessageType
	SignalType uint32
	Data       []byte
}

type MessageType string

const (
	MessageBundle MessageType = "bundle"
	MessageText   MessageType = "message"
)

// Bundle is a serializable prekey.Bundle.
type Bundle struct {
	RegID           uint32
	DeviceID        uint32
	IDKey           [32]byte
	PreKeyID        uint32
	PreKey          [32]byte
	SignedPreKeyID  uint32
	SignedPreKey    [32]byte
	SignedPreKeySig [64]byte
}

type Thread struct {
	// ID (email address) of the peer.
	ID string
	// DeviceID of the peer.
	DeviceID uint32
	// Message count.
	Messages int
	// Unread count.
	Unread int
	// Snippet is the message to be shown in
	// a thread overview.
	Snippet string
	// Updated tracks the last message time.
	Updated time.Time
	// SortKey is the sorting key for this thread.
	SortKey           string
	PendingInvitation bool
}

func OpenStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0666, nil)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		buckets := []string{"threads", "messages", "outgoing", "account", "sortedThreads"}
		for _, b := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(b)); err != nil {
				return err
			}
		}
		return s.initSignalState(tx)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func toBundle(b *prekey.Bundle) *Bundle {
	return &Bundle{
		RegID:           b.RegistrationID(),
		DeviceID:        b.DeviceID(),
		IDKey:           b.IdentityKey().PublicKey().PublicKey(),
		PreKeyID:        b.PreKeyID().Value,
		PreKey:          b.PreKey().PublicKey(),
		SignedPreKeyID:  b.SignedPreKeyID(),
		SignedPreKey:    b.SignedPreKey().PublicKey(),
		SignedPreKeySig: b.SignedPreKeySignature(),
	}
}

func fromBundle(b *Bundle) *prekey.Bundle {
	idkey := identity.NewKeyFromBytes(b.IDKey, 0)
	return prekey.NewBundle(
		b.RegID,
		b.DeviceID,
		optional.NewOptionalUint32(b.PreKeyID),
		b.SignedPreKeyID,
		ecc.NewDjbECPublicKey(b.PreKey),
		ecc.NewDjbECPublicKey(b.SignedPreKey),
		b.SignedPreKeySig,
		&idkey,
	)
}

func (s *Store) initSignalState(tx *bolt.Tx) error {
	signal := tx.Bucket([]byte("signal"))
	if signal != nil {
		return nil
	}
	signal, err := tx.CreateBucket([]byte("signal"))
	if err != nil {
		return err
	}
	ss := s.NewSignalStore(tx)
	return ss.Init()
}

func (s *Store) NewSignalStore(tx *bolt.Tx) *SignalStore {
	return &SignalStore{Bucket: tx.Bucket([]byte("signal"))}
}

func (s *Store) ContainsSession(addr string) bool {
	avail := false
	s.db.View(func(tx *bolt.Tx) error {
		threads := tx.Bucket([]byte("threads"))
		header := s.getHeader(threads.Bucket([]byte(addr)))
		ss := s.NewSignalStore(tx)
		avail = ss.ContainsSession(protocol.NewSignalAddress(header.ID, header.DeviceID))
		return nil
	})
	return avail
}

func (s *Store) GetAccount() (*Account, error) {
	var acc Account
	return &acc, s.db.View(func(tx *bolt.Tx) error {
		account := tx.Bucket([]byte("account"))
		data := account.Get([]byte("credentials"))
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &acc)
	})
}

func (s *Store) SetAccount(acc *Account) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		account := tx.Bucket([]byte("account"))
		data, err := json.Marshal(acc)
		if err != nil {
			return err
		}
		return account.Put([]byte("credentials"), data)
	})
}

func (s *Store) GetIMAPState() (*IMAPState, error) {
	state := IMAPState{
		Mailbox: "INBOX",
		// IMAP uids start at 1.
		LastUID: 1,
	}
	return &state, s.db.View(func(tx *bolt.Tx) error {
		account := tx.Bucket([]byte("account"))
		data := account.Get([]byte("imapState"))
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &state)
	})
}

func (s *Store) SetIMAPState(state *IMAPState) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		account := tx.Bucket([]byte("account"))
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return account.Put([]byte("imapState"), data)
	})
}

func (s *Store) Receive(addr string, wmsg *WireMessage) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		ss := s.NewSignalStore(tx)
		switch t := wmsg.Type; t {
		case MessageBundle:
			var b Bundle
			if err := json.Unmarshal(wmsg.Data, &b); err != nil {
				return err
			}
			sb := fromBundle(&b)
			if err := ss.ProcessBundle(addr, sb); err != nil {
				return err
			}
			m := &Message{
				Thread:  addr,
				Time:    time.Now(),
				Message: "Invitation received",
			}
			_, err := s.addMessage(tx, m, sb)
			return err
		case MessageText:
			ss := s.NewSignalStore(tx)
			thread := tx.Bucket([]byte("threads")).Bucket([]byte(addr))
			header := s.getHeader(thread)
			saddr := protocol.NewSignalAddress(addr, header.DeviceID)
			msg, err := ss.Decrypt(saddr, wmsg.SignalType, wmsg.Data)
			if err != nil {
				return err
			}
			m := &Message{
				Thread:  addr,
				Time:    time.Now(),
				Message: string(msg),
			}
			_, err = s.addMessage(tx, m, nil)
			return err
		default:
			return fmt.Errorf("unknown message type: %s", t)
		}
	})
}

func (s *Store) Send(m *Message) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		thread := tx.Bucket([]byte("threads")).Bucket([]byte(m.Thread))
		header := s.getHeader(thread)
		m.Own = true
		msgID, err := s.addMessage(tx, m, nil)
		if err != nil {
			return err
		}
		ss := s.NewSignalStore(tx)
		var wm *WireMessage
		addr := protocol.NewSignalAddress(m.Thread, header.DeviceID)
		if ss.ContainsSession(addr) {
			sigType, data, err := ss.Encrypt(addr, []byte(m.Message))
			if err != nil {
				return err
			}
			wm = &WireMessage{Type: MessageText, SignalType: sigType, Data: data}
		} else {
			bundle, err := ss.NewBundle()
			if err != nil {
				return err
			}
			data, err := json.Marshal(toBundle(bundle))
			if err != nil {
				return err
			}
			wm = &WireMessage{Type: MessageBundle, Data: data}
		}
		data, err := json.Marshal(wm)
		if err != nil {
			return err
		}
		outgoing := tx.Bucket([]byte("outgoing"))
		return outgoing.Put(itob(msgID), data)
	})
}

func (s *Store) addMessage(tx *bolt.Tx, m *Message, bundle *prekey.Bundle) (uint64, error) {
	threads := tx.Bucket([]byte("threads"))
	thread, err := threads.CreateBucketIfNotExists([]byte(m.Thread))
	if err != nil {
		return 0, err
	}
	header := s.getHeader(thread)
	header.ID = m.Thread
	header.Messages++
	header.Snippet = m.Message
	sortKey := header.SortKey
	header.SortKey = fmt.Sprintf("%d|%d", m.Time.Unix(), m.ID)
	header.Updated = m.Time
	header.Unread++
	if bundle != nil {
		header.DeviceID = bundle.DeviceID()
		header.PendingInvitation = true
	} else {
		header.PendingInvitation = false
	}
	if err := s.putHeader(thread, header); err != nil {
		return 0, err
	}
	if err := s.indexThread(tx, header.ID, sortKey, header.SortKey); err != nil {
		return 0, err
	}
	messages := tx.Bucket([]byte("messages"))
	msgID, err := messages.NextSequence()
	if err != nil {
		return 0, err
	}
	m.ID = msgID
	data, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	if err := messages.Put(itob(msgID), data); err != nil {
		return 0, err
	}
	threadMsgs, err := thread.CreateBucketIfNotExists([]byte("messages"))
	if err != nil {
		return 0, err
	}
	return msgID, threadMsgs.Put(itob(msgID), nil)
}

func (s *Store) indexThread(tx *bolt.Tx, tid, oldKey, newKey string) error {
	sortedThreads := tx.Bucket([]byte("sortedThreads"))
	if oldKey != "" {
		if err := sortedThreads.Delete([]byte(oldKey)); err != nil {
			return fmt.Errorf("indexThread: delete old key failed: %v", err)
		}
	}
	if err := sortedThreads.Put([]byte(newKey), []byte(tid)); err != nil {
		return fmt.Errorf("indexThread: add new key failed: %v", err)
	}
	return nil
}

func (s *Store) MarkRead(sender string) (bool, error) {
	marked := false
	return marked, s.db.Update(func(tx *bolt.Tx) error {
		thread := tx.Bucket([]byte("threads")).Bucket([]byte(sender))
		if thread == nil {
			return nil
		}
		header := s.getHeader(thread)
		if header.Unread == 0 {
			return nil
		}
		marked = true
		header.Unread = 0
		return s.putHeader(thread, header)
	})
}

func (s *Store) Thread(addr string) *Thread {
	var thread *Thread
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("threads")).Bucket([]byte(addr))
		thread = s.getHeader(b)
		if thread.ID == "" {
			thread.ID = addr
		}
		return nil
	})
	return thread
}

func (s *Store) Messages(sender string) ([]*Message, error) {
	var messages []*Message
	return messages, s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("threads")).Bucket([]byte(sender))
		if b == nil {
			return nil
		}
		b = b.Bucket([]byte("messages"))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		msgBucket := tx.Bucket([]byte("messages"))
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			data := msgBucket.Get(k)
			var m Message
			if err := json.Unmarshal(data, &m); err != nil {
				return err
			}
			messages = append(messages, &m)
		}
		return nil
	})
}

func (s *Store) NextOutgoing() (*Message, []byte, error) {
	var msg *Message
	var wireMsg []byte
	return msg, wireMsg, s.db.View(func(tx *bolt.Tx) error {
		outgoing := tx.Bucket([]byte("outgoing"))
		k, data := outgoing.Cursor().First()
		if k == nil {
			return nil
		}
		wireMsg = data
		msgBucket := tx.Bucket([]byte("messages"))
		data = msgBucket.Get(k)
		msg = new(Message)
		return json.Unmarshal(data, msg)
	})
}

func (s *Store) DeleteOutgoing(id uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		outgoing := tx.Bucket([]byte("outgoing"))
		k := itob(id)
		if err := outgoing.Delete(k); err != nil {
			return err
		}
		msgBucket := tx.Bucket([]byte("messages"))
		data := msgBucket.Get(k)
		msg := new(Message)
		if err := json.Unmarshal(data, msg); err != nil {
			return err
		}
		msg.Sent = true
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		return msgBucket.Put(k, data)
	})
}

func (s *Store) ThreadsByDate() ([]*Thread, error) {
	var all []*Thread
	return all, s.db.View(func(tx *bolt.Tx) error {
		sortedThreads := tx.Bucket([]byte("sortedThreads"))
		threads := tx.Bucket([]byte("threads"))
		c := sortedThreads.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			thread := threads.Bucket(v)
			header := s.getHeader(thread)
			all = append(all, header)
		}
		return nil
	})
}

func (s *Store) ThreadsByPrefix(prefix string) ([]*Thread, error) {
	p := []byte(prefix)
	var all []*Thread
	return all, s.db.View(func(tx *bolt.Tx) error {
		threads := tx.Bucket([]byte("threads"))
		c := threads.Cursor()
		for k, _ := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, _ = c.Next() {
			header := s.getHeader(threads.Bucket(k))
			all = append(all, header)
		}
		return nil
	})
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) putHeader(thread *bolt.Bucket, header *Thread) error {
	data, err := json.Marshal(header)
	if err != nil {
		return err
	}
	return thread.Put([]byte("header"), data)
}

func (s *Store) getHeader(thread *bolt.Bucket) *Thread {
	var header Thread
	if thread == nil {
		return &header
	}
	data := thread.Get([]byte("header"))
	if data == nil {
		return &header
	}
	err := json.Unmarshal(data, &header)
	if err != nil {
		// We wrote the header, so it cannot not be illformed.
		panic(err)
	}
	return &header
}

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(v))
	return b
}
