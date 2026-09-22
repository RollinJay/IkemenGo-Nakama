package transport

import (
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ikemen-engine/ggpo/internal/messages"
)

func TestUdpBindFailure(t *testing.T) {
	occupied, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	u, err := NewUdp(nil, occupied.LocalAddr().(*net.UDPAddr).Port)
	if err == nil {
		u.Close()
		t.Fatal("binding an occupied port succeeded")
	}
	if u.IsInitialized() {
		t.Fatal("failed bind left an initialized transport")
	}
	u.Close()
}

func TestUdpCloseStopsSenderAndAllowsRebind(t *testing.T) {
	port := 0
	for i := 0; i < 20; i++ {
		u, err := NewUdp(nil, port)
		if err != nil {
			t.Fatal(err)
		}
		port = u.listener.LocalAddr().(*net.UDPAddr).Port
		copyOfTransport := u
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				copyOfTransport.Close()
			}()
		}
		wg.Wait()
		select {
		case <-u.sendDone:
		default:
			t.Fatal("Close returned before sender exited")
		}
		// Sending more than the queue capacity after close must not block.
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			for j := 0; j < 512; j++ {
				u.SendTo(messages.NewUDPMessage(messages.KeepAliveMsg), "127.0.0.1", port)
			}
		}()
		select {
		case <-sent:
		case <-time.After(time.Second):
			t.Fatal("SendTo blocked after Close")
		}
	}
}

// An observed socket read makes this deterministic: Close happens only after a
// packet arrives, while no consumer is available on the delivery channel.
type observedPacketConn struct {
	net.PacketConn
	received chan struct{}
	once     sync.Once
}

func (c *observedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if n > 0 {
		c.once.Do(func() { close(c.received) })
	}
	return n, addr, err
}

func TestUdpCloseUnblocksUndeliveredPacket(t *testing.T) {
	u, err := NewUdp(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	reader := u
	observed := &observedPacketConn{PacketConn: u.listener, received: make(chan struct{})}
	reader.listener = observed
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		reader.Read(make(chan MessageChannelItem))
	}()
	sender, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: u.listener.LocalAddr().(*net.UDPAddr).Port})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if _, err := sender.Write(messages.NewUDPMessage(messages.KeepAliveMsg).ToBytes()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed.received:
	case <-time.After(time.Second):
		t.Fatal("packet did not reach reader")
	}
	u.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("reader stayed blocked delivering a packet after Close")
	}
}

func TestNewUdpFromPacketConnUsesSuppliedSocket(t *testing.T) {
	supplied, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := supplied.LocalAddr().(*net.UDPAddr).Port

	u := NewUdpFromPacketConn(nil, supplied)
	if !u.IsInitialized() {
		t.Fatal("packet-conn transport is not initialized")
	}
	if got := u.listener.LocalAddr().(*net.UDPAddr).Port; got != port {
		t.Fatalf("transport changed local port: got %d want %d", got, port)
	}

	occupied, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		u.Close()
		t.Fatal(err)
	}
	occupiedPort := occupied.LocalAddr().(*net.UDPAddr).Port
	occupied.Close()
	_ = occupiedPort

	probe, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		u.Close()
		t.Fatal(err)
	}
	defer probe.Close()

	readDone := make(chan struct{})
	messagesOut := make(chan MessageChannelItem, 1)
	go func() {
		u.Read(messagesOut)
		close(readDone)
	}()
	if _, err := probe.Write(messages.NewUDPMessage(messages.KeepAliveMsg).ToBytes()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-messagesOut:
	case <-time.After(time.Second):
		t.Fatal("packet did not arrive through supplied PacketConn")
	}
	u.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("transport reader did not exit after close")
	}

	rebound, err := net.ListenPacket("udp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("supplied socket was not closed by transport: %v", err)
	}
	rebound.Close()
}
