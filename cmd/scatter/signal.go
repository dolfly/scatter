// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eliasnaur/libsignal-protocol-go/ecc"
	"github.com/eliasnaur/libsignal-protocol-go/keys/identity"
	"github.com/eliasnaur/libsignal-protocol-go/keys/prekey"
	"github.com/eliasnaur/libsignal-protocol-go/protocol"
	"github.com/eliasnaur/libsignal-protocol-go/serialize"
	"github.com/eliasnaur/libsignal-protocol-go/session"
	"github.com/eliasnaur/libsignal-protocol-go/state/record"
	"github.com/eliasnaur/libsignal-protocol-go/util/keyhelper"
	bolt "go.etcd.io/bbolt"
)

type ID struct {
	RegID      uint32
	DeviceID   uint32
	PublicKey  [32]byte
	PrivateKey [32]byte
}

type SignalStore struct {
	Bucket *bolt.Bucket
	Err    error
}

func (s *SignalStore) GetIdentityKeyPair() *identity.KeyPair {
	data := s.Bucket.Get([]byte("id"))
	var id ID
	if err := json.Unmarshal(data, &id); err != nil {
		s.Err = err
		return nil
	}
	pub := identity.NewKeyFromBytes(id.PublicKey, 0)
	priv := ecc.NewDjbECPrivateKey(id.PrivateKey)
	return identity.NewKeyPair(&pub, priv)
}

func (s *SignalStore) GetLocalRegistrationId() uint32 {
	data := s.Bucket.Get([]byte("id"))
	var id ID
	s.Err = json.Unmarshal(data, &id)
	return id.RegID
}

func (s *SignalStore) getID() *ID {
	data := s.Bucket.Get([]byte("id"))
	var id ID
	s.Err = json.Unmarshal(data, &id)
	return &id
}

func (s *SignalStore) SaveIdentity(addr *protocol.SignalAddress, key *identity.Key) {
	ids, err := s.Bucket.CreateBucketIfNotExists([]byte("identities"))
	if err != nil {
		s.Err = err
		return
	}
	pub := key.PublicKey().PublicKey()
	s.Err = ids.Put([]byte(addr.String()), pub[:])
}

func (s *SignalStore) IsTrustedIdentity(addr *protocol.SignalAddress, id *identity.Key) bool {
	ids := s.Bucket.Bucket([]byte("identities"))
	if ids == nil {
		return true
	}
	trustedData := ids.Get([]byte(addr.String()))
	if trustedData == nil {
		return true
	}
	if len(trustedData) != 32 {
		panic("invalid key")
	}
	var keyBytes [32]byte
	copy(keyBytes[:], trustedData)
	trusted := identity.NewKeyFromBytes(keyBytes, 0)
	return trusted.Fingerprint() == id.Fingerprint()
}

func (s *SignalStore) LoadSignedPreKey(keyID uint32) *record.SignedPreKey {
	skeys := s.Bucket.Bucket([]byte("signedPreKeys"))
	if skeys == nil {
		return nil
	}
	data := skeys.Get(itob(uint64(keyID)))
	if data == nil {
		return nil
	}
	pk, err := unmarshalSignedPreKey(data)
	s.Err = err
	return pk
}

func unmarshalSignedPreKey(data []byte) (*record.SignedPreKey, error) {
	serializer := serialize.NewJSONSerializer()
	pkStruct, err := serializer.SignedPreKeyRecord.Deserialize(data)
	if err != nil {
		return nil, err
	}
	// BUG: The JSON signed prekey serializer is not idempotent: it adds the key type (5)
	// and cuts off the last key byte every roundtrip. Chop the type off before continuing.
	pkStruct.PublicKey = pkStruct.PublicKey[1:]
	return record.NewSignedPreKeyFromStruct(pkStruct, serializer.SignedPreKeyRecord)
}

func (s *SignalStore) StoreSignedPreKey(keyID uint32, record *record.SignedPreKey) {
	skeys, err := s.Bucket.CreateBucketIfNotExists([]byte("signedPreKeys"))
	if err != nil {
		s.Err = err
		return
	}
	data := record.Serialize()
	s.Err = skeys.Put(itob(uint64(keyID)), data)
}

func (s *SignalStore) LoadSignedPreKeys() []*record.SignedPreKey {
	skeys := s.Bucket.Bucket([]byte("signedPreKeys"))
	if skeys == nil {
		return nil
	}
	var keys []*record.SignedPreKey
	c := skeys.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		pk, err := unmarshalSignedPreKey(v)
		if err != nil {
			s.Err = err
			return keys
		}
		keys = append(keys, pk)
	}
	return keys
}

func (s *SignalStore) ContainsSignedPreKey(keyID uint32) bool {
	return s.LoadSignedPreKey(keyID) != nil
}

func (s *SignalStore) RemoveSignedPreKey(keyID uint32) {
	skeys := s.Bucket.Bucket([]byte("signedPreKeys"))
	if skeys == nil {
		return
	}
	s.Err = skeys.Delete(itob(uint64(keyID)))
}

func (s *SignalStore) StorePreKey(keyID uint32, keyRec *record.PreKey) {
	keys, err := s.Bucket.CreateBucketIfNotExists([]byte("preKeys"))
	if err != nil {
		s.Err = err
		return
	}
	s.Err = keys.Put(itob(uint64(keyID)), keyRec.Serialize())
}

func (s *SignalStore) ContainsPreKey(keyID uint32) bool {
	return s.LoadPreKey(keyID) != nil
}

func (s *SignalStore) RemovePreKey(keyID uint32) {
	keys := s.Bucket.Bucket([]byte("preKeys"))
	if keys == nil {
		return
	}
	s.Err = keys.Delete(itob(uint64(keyID)))
}

func (s *SignalStore) LoadPreKey(keyID uint32) *record.PreKey {
	keys := s.Bucket.Bucket([]byte("preKeys"))
	if keys == nil {
		return nil
	}
	data := keys.Get(itob(uint64(keyID)))
	if data == nil {
		return nil
	}
	serializer := serialize.NewJSONSerializer()
	key, err := record.NewPreKeyFromBytes(data, serializer.PreKeyRecord)
	s.Err = err
	return key
}

func (s *SignalStore) LoadSession(addr *protocol.SignalAddress) *record.Session {
	sess, err := s.loadSession(addr)
	if err != nil {
		s.Err = err
		return nil
	}
	if sess == nil {
		serializer := serialize.NewJSONSerializer()
		sess = record.NewSession(serializer.Session, serializer.State)
	}
	return sess
}

func (s *SignalStore) loadSession(addr *protocol.SignalAddress) (*record.Session, error) {
	sessions := s.Bucket.Bucket([]byte("sessions"))
	if sessions == nil {
		return nil, nil
	}
	serializer := serialize.NewJSONSerializer()
	data := sessions.Get([]byte(addr.String()))
	if data == nil {
		return nil, nil
	}
	sess, err := record.NewSessionFromBytes(data, serializer.Session, serializer.State)
	s.Err = err
	return sess, nil
}

func (s *SignalStore) GetSubDeviceSessions(name string) []uint32 {
	panic("unimplemented")
}

func (s *SignalStore) StoreSession(addr *protocol.SignalAddress, record *record.Session) {
	sessions, err := s.Bucket.CreateBucketIfNotExists([]byte("sessions"))
	if err != nil {
		s.Err = err
		return
	}
	s.Err = sessions.Put([]byte(addr.String()), record.Serialize())
}

func (s *SignalStore) ContainsSession(addr *protocol.SignalAddress) bool {
	sess, err := s.loadSession(addr)
	s.Err = err
	return sess != nil
}

func (s *SignalStore) DeleteSession(addr *protocol.SignalAddress) {
	sessions := s.Bucket.Bucket([]byte("sessions"))
	if sessions == nil {
		return
	}
	s.Err = sessions.Delete([]byte(addr.String()))
}

func (s *SignalStore) DeleteAllSessions() {
	s.Err = s.Bucket.DeleteBucket([]byte("sessions"))
}

func (s *SignalStore) ProcessBundle(addr string, bundle *prekey.Bundle) error {
	saddr := protocol.NewSignalAddress(addr, bundle.DeviceID())
	return s.newSessionBuilder(saddr).ProcessBundle(bundle)
}

func (s *SignalStore) newSessionBuilder(addr *protocol.SignalAddress) *session.Builder {
	serializer := serialize.NewJSONSerializer()
	return session.NewBuilder(
		s,
		s,
		s,
		s,
		addr,
		serializer,
	)
}

func (s *SignalStore) NewPreKey() (*record.PreKey, error) {
	keys, err := s.Bucket.CreateBucketIfNotExists([]byte("preKeys"))
	if err != nil {
		return nil, err
	}
	keyID, err := keys.NextSequence()
	if err != nil {
		return nil, err
	}
	keyID32 := uint32(keyID)
	if uint64(keyID32) != keyID {
		return nil, errors.New("prekey id overflow")
	}
	keyPair, err := ecc.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	serializer := serialize.NewJSONSerializer()
	preKey := record.NewPreKey(keyID32, keyPair, serializer.PreKeyRecord)
	s.StorePreKey(keyID32, preKey)
	return preKey, s.Err
}

func (s *SignalStore) Init() error {
	idKeyPair, err := keyhelper.GenerateIdentityKeyPair()
	if err != nil {
		return err
	}
	priv := idKeyPair.PrivateKey().Serialize()
	id := &ID{
		RegID:      keyhelper.GenerateRegistrationID(),
		DeviceID:   keyhelper.GenerateRegistrationID(),
		PrivateKey: priv,
		PublicKey:  idKeyPair.PublicKey().PublicKey().PublicKey(),
	}
	data, err := json.Marshal(id)
	if err != nil {
		return err
	}
	if err := s.Bucket.Put([]byte("id"), data); err != nil {
		return err
	}
	serializer := serialize.NewJSONSerializer()
	signedPreKey, err := keyhelper.GenerateSignedPreKey(idKeyPair, 0, serializer.SignedPreKeyRecord)
	if err != nil {
		return err
	}
	s.StoreSignedPreKey(
		signedPreKey.ID(),
		record.NewSignedPreKey(
			signedPreKey.ID(),
			signedPreKey.Timestamp(),
			signedPreKey.KeyPair(),
			signedPreKey.Signature(),
			serializer.SignedPreKeyRecord,
		),
	)
	return s.Err
}

func (s *SignalStore) Decrypt(addr *protocol.SignalAddress, stype uint32, data []byte) ([]byte, error) {
	serializer := serialize.NewJSONSerializer()
	var message *protocol.SignalMessage
	var builder *session.Builder
	switch stype {
	case protocol.PREKEY_TYPE:
		msg, err := protocol.NewPreKeySignalMessageFromBytes(data, serializer.PreKeySignalMessage, serializer.SignalMessage)
		if err != nil {
			return nil, err
		}
		builder = s.newSessionBuilder(addr)
		sess := s.LoadSession(addr)
		if _, err := builder.Process(sess, msg); err != nil {
			return nil, err
		}
		s.StoreSession(addr, sess)
		message = msg.WhisperMessage()
	case protocol.WHISPER_TYPE:
		msg, err := protocol.NewSignalMessageFromBytes(data, serializer.SignalMessage)
		if err != nil {
			return nil, err
		}
		message = msg
		builder = s.newSessionBuilder(addr)
	default:
		return nil, fmt.Errorf("decrypt: unknown signal message type: %v", stype)
	}
	cipher := session.NewCipher(builder, addr)
	return cipher.Decrypt(message)
}

func (s *SignalStore) Encrypt(addr *protocol.SignalAddress, msg []byte) (uint32, []byte, error) {
	cipher, err := s.newCipher(addr)
	if err != nil {
		return 0, nil, err
	}
	data, err := cipher.Encrypt(msg)
	if err != nil {
		return 0, nil, err
	}
	return data.Type(), data.Serialize(), nil
}

func (s *SignalStore) newCipher(addr *protocol.SignalAddress) (*session.Cipher, error) {
	serializer := serialize.NewJSONSerializer()
	cipher := session.NewCipherFromSession(addr, s, s, s, serializer.PreKeySignalMessage, serializer.SignalMessage)
	return cipher, nil
}

func (s *SignalStore) NewBundle() (*prekey.Bundle, error) {
	pk, err := s.NewPreKey()
	if err != nil {
		return nil, err
	}
	spk, err := s.LoadSignedPreKey(0), s.Err
	if err != nil {
		return nil, err
	}
	idkp, err := s.GetIdentityKeyPair(), s.Err
	if err != nil {
		return nil, err
	}
	id := s.getID()
	b := prekey.NewBundle(
		id.RegID,
		id.DeviceID,
		pk.ID(),
		spk.ID(),
		pk.KeyPair().PublicKey(),
		spk.KeyPair().PublicKey(),
		spk.Signature(),
		idkp.PublicKey(),
	)
	return b, nil
}
