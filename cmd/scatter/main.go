// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gioui.org/ui/app"
)

var (
	imapHost = flag.String("imap", "", "specify the IMAP host and port")
	smtpHost = flag.String("smtp", "", "specify the SMTP host and port")
	user     = flag.String("user", "", "specify the username (e.g. user@example.com)")
	pass     = flag.String("pass", "", "specify the password")

	dataDir string
)

var (
	clientOnce   sync.Once
	singleClient *Client
)

func init() {
	log.SetPrefix("scatter: ")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Scatter is a federated implementation of the Signal protocol on email.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n\n\tscatter [flags] <pkg>\n\n")
		flag.PrintDefaults()
		os.Exit(2)
	}
	conf, err := os.UserConfigDir()
	if err == nil {
		conf = filepath.Join(conf, "scatter")
	}
	flag.StringVar(&dataDir, "store", conf, "directory for storing configuration and messages")
	flag.Parse()
}

func getClient() *Client {
	clientOnce.Do(func() {
		cl, err := initClient()
		if err != nil {
			errorf("scatter: %v", err)
		}
		singleClient = cl
	})
	return singleClient
}

func initClient() (*Client, error) {
	dir := dataDir
	if dir == "" {
		var err error
		dir, err = app.DataDir()
		if err != nil {
			errorf("scatter: %v", err)
		}
		dir = filepath.Join(dir, "scatter")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	store, err := OpenStore(filepath.Join(dir, "store.db"))
	if err != nil {
		return nil, err
	}
	acc, err := store.GetAccount()
	if err != nil {
		store.Close()
		return nil, err
	}
	if *imapHost != "" {
		acc.IMAPHost = *imapHost
	}
	if *smtpHost != "" {
		acc.SMTPHost = *smtpHost
	}
	if *user != "" {
		acc.User = *user
	}
	if *pass != "" {
		acc.Password = *pass
	}
	store.SetAccount(acc)
	cl, err := NewClient(store)
	if err != nil {
		store.Close()
		return nil, err
	}
	cl.Run()
	singleClient = cl
	return cl, nil
}

func main() {
	uiMain()
}

func errorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	time.Sleep(5 * time.Second)
	os.Exit(1)
}
