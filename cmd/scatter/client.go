// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	idle "github.com/emersion/go-imap-idle"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type Client struct {
	cancel  context.CancelFunc
	account *Account

	flushC chan struct{}
	store  *Store

	state *IMAPState

	mu        sync.Mutex
	listeners map[interface{}]chan<- struct{}
	err       error
}

func NewClient(store *Store) (*Client, error) {
	acc, err := store.GetAccount()
	if err != nil {
		return nil, err
	}
	state, err := store.GetIMAPState()
	if err != nil {
		return nil, err
	}
	c := &Client{
		account:   acc,
		state:     state,
		store:     store,
		flushC:    make(chan struct{}, 1),
		listeners: make(map[interface{}]chan<- struct{}),
	}
	// Send messages left over from last run.
	c.flushC <- struct{}{}
	return c, nil
}

func (c *Client) Run() {
	go c.runOutgoing()
	if c.cancel == nil {
		c.restartIncoming()
	}
}

func (c *Client) ContainsSession(addr string) bool {
	return c.store.ContainsSession(addr)
}

func (s *Client) Thread(addr string) *Thread {
	return s.store.Thread(addr)
}

func (c *Client) Account() *Account {
	acc := *c.account
	return &acc
}

func (c *Client) SetAccount(acc *Account) error {
	copy := *acc
	c.account = &copy
	if err := c.store.SetAccount(acc); err != nil {
		return err
	}
	c.restartIncoming()
	return nil
}

func (c *Client) restartIncoming() {
	if c.cancel != nil {
		c.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.runIncoming(ctx)
}

func (c *Client) Threads() ([]*Thread, error) {
	return c.store.ThreadsByDate()
}

func (c *Client) QueryThreads(q string) ([]*Thread, error) {
	return c.store.ThreadsByPrefix(q)
}

func (c *Client) Messages(sender string) ([]*Message, error) {
	return c.store.Messages(sender)
}

func (c *Client) List() error {
	threads, err := c.Threads()
	if err != nil {
		return err
	}
	log.Println("Threads:")
	for _, t := range threads {
		log.Printf("%s (%d)", t.ID, t.Messages)
		msgs, err := c.Messages(t.ID)
		if err != nil {
			return nil
		}
		for _, m := range msgs {
			log.Printf("%s: %s", m.Time, m.Message)
		}
	}
	return nil
}

func (c *Client) Close() error {
	return c.store.Close()
}

func (c *Client) runIncoming(ctx context.Context) {
	var retry time.Duration
	acc := c.Account()
	for {
		log.Printf("connecting to %s", acc.IMAPHost)
		cl, err := client.DialTLS(acc.IMAPHost, nil)
		if err != nil {
			log.Printf("imap connection failed: %v", err)
			if err, ok := err.(*net.OpError); ok && err.Temporary() {
				backoff(&retry)
				continue
			} else {
				c.setError(err)
				break
			}
		}
		retry = 0
		defer cl.Logout()
		log.Printf("connected to %s", acc.IMAPHost)
		if err := cl.Login(acc.User, acc.Password); err != nil {
			log.Printf("imap login failed: %v", err)
			c.setError(err)
			break
		}
		mb, err := cl.Select(c.state.Mailbox, false)
		if err != nil {
			log.Printf("selecting mailbox %s failed: %v", c.state.Mailbox, err)
			c.setError(err)
			break
		}
		c.updateValidity(mb.UidValidity)
		log.Printf("signed in to %s", acc.IMAPHost)
		stop, err := c.listen(ctx, cl)
		if err != nil {
			log.Printf("imap connection lost: %v", err)
		}
		if stop {
			break
		}
	}
}

func (c *Client) updateValidity(uidValidity uint32) {
	if uidValidity != c.state.UIDValidity {
		c.state.LastUID = 1
	}
	c.state.UIDValidity = uidValidity
	c.store.SetIMAPState(c.state)
}

func (c *Client) listen(ctx context.Context, cl *client.Client) (bool, error) {
	notify := make(chan *imap.MailboxStatus, 1)
	updates := make(chan client.Update)
	cl.Updates = updates
	go func() {
		for upd := range updates {
			switch u := upd.(type) {
			case *client.MailboxUpdate:
				select {
				case notify <- u.Mailbox:
				default:
				}
			}
		}
	}()
	if err := c.fetchNewMessages(cl); err != nil {
		return false, err
	}
	done, stop := c.idle(cl)

	for {
		select {
		case err := <-done:
			if err != nil {
				return false, err
			}
		case mb := <-notify:
			stop <- struct{}{}
			if err := <-done; err != nil {
				return false, err
			}
			c.updateValidity(mb.UidValidity)
			if err := c.fetchNewMessages(cl); err != nil {
				return false, err
			}
			if mb.UidNext > c.state.LastUID {
				c.state.LastUID = mb.UidNext
				c.store.SetIMAPState(c.state)
			}
		case <-ctx.Done():
			return true, ctx.Err()
		}
		done, stop = c.idle(cl)
	}
}

func (c *Client) idle(cl *client.Client) (<-chan error, chan<- struct{}) {
	idleClient := idle.NewClient(cl)
	done := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		done <- idleClient.IdleWithFallback(stop, 0)
	}()
	return done, stop
}

func (c *Client) fetchNewMessages(cl *client.Client) error {
	allUids := new(imap.SeqSet)
	allUids.AddRange(c.state.LastUID, ^uint32(0))
	ids, err := cl.UidSearch(&imap.SearchCriteria{
		Uid: allUids,
		Header: textproto.MIMEHeader{
			"X-Scatter-Msg": []string{""},
		},
	})
	if err != nil {
		return err
	}

	if len(ids) == 0 {
		return nil
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)

	var section imap.BodySectionName
	messages := make(chan *imap.Message, len(ids))
	done := make(chan error, 1)
	go func() {
		done <- cl.UidFetch(seqset, []imap.FetchItem{section.FetchItem(), imap.FetchUid, imap.FetchEnvelope}, messages)
	}()

	delSet := new(imap.SeqSet)
loop:
	for msg := range messages {
		if msg.Envelope == nil {
			continue
		}
		if msg.Uid >= c.state.LastUID {
			c.state.LastUID = msg.Uid + 1
			c.store.SetIMAPState(c.state)
		}
		sender := msg.Envelope.From[0]
		r := msg.GetBody(&section)
		if r == nil {
			continue
		}

		// Create a new mail reader
		mr, err := mail.CreateReader(r)
		if err != nil {
			log.Printf("failed to read message body: %v", err)
			continue
		}

		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			} else if err != nil {
				log.Printf("failed to read message part: %v", err)
				continue loop
			}
			switch h := p.Header.(type) {
			case *mail.AttachmentHeader:
				filename, err := h.Filename()
				if err != nil {
					log.Printf("failed to read attachment: %v", err)
					continue loop
				}
				if filename != "message" {
					continue loop
				}
				addr := fmt.Sprintf("%s@%s", sender.MailboxName, sender.HostName)
				if err := c.receive(addr, p.Body); err != nil {
					log.Printf("failed to parse message: %v", err)
					continue loop
				}
			}
		}
		delSet.AddNum(msg.Uid)
	}

	if err := <-done; err != nil {
		return err
	}
	if delSet.Empty() {
		return nil
	}
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.DeletedFlag}
	if err = cl.UidStore(delSet, item, flags, nil); err != nil {
		return err
	}
	if err := cl.Expunge(nil); err != nil {
		return err
	}
	return nil
}

func (c *Client) register(key interface{}) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.listeners[key]; exists {
		panic("only one registration per key is allowed")
	}
	ch := make(chan struct{}, 1)
	c.listeners[key] = ch
	return ch
}

func (c *Client) unregister(key interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.listeners[key]; !exists {
		panic("no registration found")
	}
	delete(c.listeners, key)
}

func (c *Client) receive(addr string, msg io.Reader) error {
	var wmsg WireMessage
	if err := json.NewDecoder(msg).Decode(&wmsg); err != nil {
		return err
	}
	if err := c.store.Receive(addr, &wmsg); err != nil {
		return err
	}
	c.notify()
	return nil
}

func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	c.err = nil
	return err
}

func (c *Client) setError(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	c.notify()
}

func (c *Client) notify() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.listeners {
		select {
		case l <- struct{}{}:
		default:
		}
	}
}

func (c *Client) Send(addr, body string) error {
	err := c.store.Send(&Message{
		Thread:  addr,
		Own:     true,
		Time:    time.Now(),
		Message: body,
	})
	if err != nil {
		return err
	}
	c.notify()
	c.flush()
	return nil
}

func (c *Client) MarkRead(addr string) error {
	marked, err := c.store.MarkRead(addr)
	if err != nil {
		return err
	}
	if marked {
		c.notify()
	}
	return nil
}

func (c *Client) flush() {
	select {
	case c.flushC <- struct{}{}:
	default:
	}
}

func (c *Client) runOutgoing() {
	var retry time.Duration
	for range c.flushC {
		for {
			msg, wmsg, err := c.store.NextOutgoing()
			if err != nil {
				log.Printf("failed to fetch outgoing message: %v", err)
				break
			}
			if msg == nil {
				break
			}
			if err := c.send(msg, wmsg); err != nil {
				log.Printf("failed to send message: %v", err)
				backoff(&retry)
				continue
			}
			retry = 0
			if err := c.store.DeleteOutgoing(msg.ID); err != nil {
				log.Printf("failed to delete outgoing message: %v", err)
				break
			}
			c.notify()
		}
	}
}

func backoff(sleep *time.Duration) {
	if *sleep < time.Second {
		*sleep = time.Second
	}
	time.Sleep(*sleep)
	*sleep *= 2
	if max := time.Minute; *sleep > max {
		*sleep = max
	}
}

func (c *Client) send(msg *Message, wireMsg []byte) error {
	const marker = "ATTACHMENTMARKER"
	const maxLineLen = 500

	auth := sasl.NewPlainClient("", c.account.User, c.account.Password)
	var buf bytes.Buffer
	buf.WriteString("From: " + *user + "\r\n" +
		"From: " + *user + "\r\n" +
		"Subject: New Scatter message\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=" + marker + "\r\n" +
		"X-Scatter-Msg: \r\n" +
		"--" + marker + "\r\n" +
		"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" +
		"Hello,\r\n" +
		"\r\n" +
		"This is an encrypted message sent with Scatter. Download the app from https://scatter.im to open it.\r\n" +
		"--" + marker + "\r\n" +
		"Content-Type: application/octet-stream; name=\"message\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"message\"\r\n\r\n")
	encMsg := base64.StdEncoding.EncodeToString(wireMsg)
	for len(encMsg) > 0 {
		n := maxLineLen
		if n > len(encMsg) {
			n = len(encMsg)
		}
		buf.WriteString(encMsg[:n])
		buf.WriteString("\r\n")
		encMsg = encMsg[n:]
	}
	buf.WriteString("--" + marker + "--\r\n")
	return smtp.SendMail(c.account.SMTPHost, auth, c.account.User, []string{msg.Thread}, &buf)
}
