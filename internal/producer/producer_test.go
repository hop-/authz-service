package producer

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// listen starts a local TCP listener that accepts and closes connections until
// the test ends.
func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	return ln.Addr().String()
}

// nonRoutable returns a TEST-NET-1 address (RFC 5737, 192.0.2.0/24): never
// assigned, so a dial gets no answer and fails only at its deadline. Unlike a
// released local port, no other process can take it (007 F-008).
func nonRoutable(n int) string {
	return fmt.Sprintf("192.0.2.%d:9", n)
}

// refused returns an address whose dial fails at once: nothing can listen on
// port 0, so the kernel refuses the connection.
func refused() string {
	return "127.0.0.1:0"
}

func TestNewProducer_KeepsBrokerList(t *testing.T) {
	p := NewProducer("a:9092,b:9092", "topic")
	defer p.Close()
	if len(p.brokers) != 2 || p.brokers[0] != "a:9092" || p.brokers[1] != "b:9092" {
		t.Fatalf("brokers = %v", p.brokers)
	}
}

func TestPing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Ping dials brokers in order on one ctx, so the first broker must fail
	// fast here; a non-routable one would use up the whole deadline.
	t.Run("first unreachable, second reachable", func(t *testing.T) {
		bad := refused()
		good := listen(t)
		p := NewProducer(bad+","+good, "topic")
		defer p.Close()

		if err := p.Ping(ctx); err != nil {
			t.Fatalf("Ping = %v, want nil when any broker is reachable", err)
		}
	})

	t.Run("single reachable", func(t *testing.T) {
		p := NewProducer(listen(t), "topic")
		defer p.Close()

		if err := p.Ping(ctx); err != nil {
			t.Fatalf("Ping = %v, want nil", err)
		}
	})

	t.Run("all unreachable", func(t *testing.T) {
		bad1 := nonRoutable(1)
		bad2 := nonRoutable(2)
		p := NewProducer(bad1+","+bad2, "topic")
		defer p.Close()
		short, cancelShort := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancelShort()

		start := time.Now()
		err := p.Ping(short)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Ping took %v, want it bounded by the 300ms deadline", elapsed)
		}
		if err == nil {
			t.Fatal("Ping = nil, want error when no broker is reachable")
		}
		// errors.Join of every broker's failure.
		for _, addr := range []string{bad1, bad2} {
			if !strings.Contains(err.Error(), addr) {
				t.Errorf("error %q does not mention broker %s", err, addr)
			}
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		cctx, ccancel := context.WithCancel(context.Background())
		ccancel()
		p := NewProducer(listen(t), "topic")
		defer p.Close()

		if err := p.Ping(cctx); err == nil {
			t.Fatal("Ping with a cancelled context = nil, want error")
		}
	})
}

// TestNewProducer_DLQWriterCreatedOnce covers AC-1: the DLQ writer is built
// once, next to the main writer, and closed with it.
func TestNewProducer_DLQWriterCreatedOnce(t *testing.T) {
	p := NewProducer("127.0.0.1:1,127.0.0.1:2", "authz.permission-events")

	if p.dlqWriter == nil {
		t.Fatal("DLQ writer not created by NewProducer")
	}
	if p.dlqWriter.Topic != "authz.permission-events.dlq" {
		t.Errorf("DLQ writer topic = %q, want authz.permission-events.dlq", p.dlqWriter.Topic)
	}
	if p.writer.Topic != "authz.permission-events" {
		t.Errorf("main writer topic = %q", p.writer.Topic)
	}
	if p.dlqWriter.Addr.String() != p.writer.Addr.String() {
		t.Errorf("DLQ writer brokers %q differ from main writer %q", p.dlqWriter.Addr, p.writer.Addr)
	}
	if p.dlqWriter == p.writer {
		t.Error("DLQ writer must be a separate writer")
	}

	// A failed publish must not replace the writer: the same instance serves
	// every call.
	before := p.dlqWriter
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.PublishDLQ(ctx, "evt-1", map[string]string{"k": "v"}); err == nil {
		t.Fatal("PublishDLQ to an unreachable broker succeeded")
	}
	if p.dlqWriter != before {
		t.Error("PublishDLQ replaced the DLQ writer")
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	// kafka.Writer.WriteMessages on a closed writer fails with io.ErrClosedPipe.
	if err := p.PublishDLQ(context.Background(), "evt-2", map[string]string{"k": "v"}); err == nil {
		t.Error("PublishDLQ after Close succeeded; Close must close the DLQ writer")
	}
}
