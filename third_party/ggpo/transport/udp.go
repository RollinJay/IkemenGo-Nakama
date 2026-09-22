package transport

import (
	"net"
	"strconv"
	"sync"

	"github.com/ikemen-engine/ggpo/internal/messages"
	"github.com/ikemen-engine/ggpo/internal/util"
)

const (
	MaxUDPEndpoints  = 16
	MaxUDPPacketSize = 4096
)

type Udp struct {
	Stats UdpStats // may not need this, may just be a service used by others

	socket         net.Conn
	messageHandler MessageHandler
	listener       net.PacketConn
	localPort      int
	ipAddress      string
	sendChan       chan sendRequest
	done           chan struct{}
	sendDone       chan struct{}
	closeOnce      *sync.Once // Shared by the value copies stored in endpoints.
}

type UdpStats struct {
	BytesSent   int
	PacketsSent int
	KbpsSent    float64
}

func getPeerAddress(address net.Addr) peerAddress {
	switch addr := address.(type) {
	case *net.UDPAddr:
		return peerAddress{
			Ip:   addr.IP.String(),
			Port: addr.Port,
		}
	}
	return peerAddress{}
}

func (u Udp) Close() {
	if u.closeOnce != nil {
		u.closeOnce.Do(func() {
			close(u.done)
			u.listener.Close()
			<-u.sendDone
		})
	}
}

type sendRequest struct {
	msg        messages.UDPMessage
	remoteIp   string
	remotePort int
}

func NewUdp(messageHandler MessageHandler, localPort int) (Udp, error) {
	util.Log.Printf("binding udp socket to port %d.\n", localPort)
	listener, err := net.ListenPacket("udp", "0.0.0.0:"+strconv.Itoa(localPort))
	if err != nil {
		return Udp{}, err
	}
	return newUdpFromListener(messageHandler, listener), nil
}

// NewUdpFromPacketConn creates a GGPO UDP transport over an already-bound
// packet connection. Ownership of listener transfers to the returned Udp.
// This is used by callers that must preserve a socket established before GGPO
// starts, for example after NAT traversal.
func NewUdpFromPacketConn(messageHandler MessageHandler, listener net.PacketConn) Udp {
	return newUdpFromListener(messageHandler, listener)
}

func newUdpFromListener(messageHandler MessageHandler, listener net.PacketConn) Udp {
	u := Udp{
		messageHandler: messageHandler,
		listener:       listener,
		sendChan:       make(chan sendRequest, 256),
		done:           make(chan struct{}),
		sendDone:       make(chan struct{}),
		closeOnce:      &sync.Once{},
	}
	if listener != nil && listener.LocalAddr() != nil {
		switch addr := listener.LocalAddr().(type) {
		case *net.UDPAddr:
			u.localPort = addr.Port
		default:
			// The transport only needs localPort for diagnostics/legacy state;
			// packet I/O itself is performed through the PacketConn interface.
		}
	}

	go func() {
		defer close(u.sendDone)
		for {
			select {
			case <-u.done:
				return
			case req := <-u.sendChan:
				if u.listener == nil {
					continue
				}
				RemoteEP := net.UDPAddr{IP: net.ParseIP(req.remoteIp), Port: req.remotePort}
				buf := req.msg.ToBytes()
				_, err := u.listener.WriteTo(buf, &RemoteEP)
				if err != nil {
					util.Log.Printf("WriteTo error: %s", err)
				}
			}
		}
	}()

	return u
}

// dst should be sockaddr
// maybe create Gob encoder and decoder members
// instead of creating them on each message send
func (u Udp) SendTo(msg messages.UDPMessage, remoteIp string, remotePort int) {
	if msg == nil || remoteIp == "" || u.sendChan == nil {
		return
	}

	// RemoteEP := net.UDPAddr{IP: net.ParseIP(remoteIp), Port: remotePort}
	// buf := msg.ToBytes()
	// u.listener.WriteTo(buf, &RemoteEP)
	select {
	case <-u.done:
		return
	default:
	}
	select {
	case <-u.done:
	case u.sendChan <- sendRequest{msg: msg, remoteIp: remoteIp, remotePort: remotePort}:
	}
}

func (u Udp) Read(messageChan chan MessageChannelItem) {
	if u.listener == nil {
		return
	}
	defer u.Close()
	recvBuf := make([]byte, MaxUDPPacketSize*2)
	for {
		len, addr, err := u.listener.ReadFrom(recvBuf)
		if err != nil {
			util.Log.Printf("conn.Read error returned: %s\n", err)
			break
		} else if len <= 0 {
			util.Log.Printf("no data recieved\n")
		} else if len > 0 {
			util.Log.Printf("recvfrom returned (len:%d  from:%s).\n", len, addr.String())
			peer := getPeerAddress(addr)

			msg, err := messages.DecodeMessageBinary(recvBuf)
			if err != nil {
				util.Log.Printf("Error decoding message: %s", err)
				continue
			}
			// The game may have stopped pumping GGPO before closing this session.
			select {
			case <-u.done:
				return
			case messageChan <- MessageChannelItem{Peer: peer, Message: msg, Length: len}:
			}
		}

	}
}

func (u Udp) IsInitialized() bool {
	return u.listener != nil
}
