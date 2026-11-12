package store

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestMySQLConnectHonorsContextDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	// Accept TCP but deliberately never send the MySQL handshake.
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-done
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	st, err := OpenMySQLContext(ctx, "test@tcp("+listener.Addr().String()+")/test?parseTime=true")
	if st != nil {
		_ = st.Close()
		t.Fatal("unexpected successful connection")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected handshake deadline, got %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("connection exceeded context budget")
	}
}
