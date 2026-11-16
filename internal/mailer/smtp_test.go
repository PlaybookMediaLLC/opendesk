package mailer

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestSMTPSendStopsWithItsContext: a server that accepts the connection and never answers
// does not hold the sender past its context.
func TestSMTPSendStopsWithItsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // never greets
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(port)
	m := &smtpMailer{o: Options{From: "desk@acme.test", Host: "127.0.0.1", Port: n, TLS: "none"}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := m.Send(ctx, Mail{To: "a@b.test", Subject: "Hi", Text: "Hi"}); err == nil {
		t.Fatal("a silent server accepted the mail")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("send waited %s on a silent server", d)
	}
}
