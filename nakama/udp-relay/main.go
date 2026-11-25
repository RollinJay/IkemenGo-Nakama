// Command udp-relay is the UDP relay of IKEMEN GO's P2P sessions: the
// fallback of two players whose games find no direct path to each other,
// without the stutter of the websocket relay. See README.md and package
// udprelay.
//
//	udp-relay -listen :17360 -secret-file ~/.config/ikemen-udp-relay/secret
//
// The secret is also read from IKEMEN_UDP_RELAY_SECRET; it must be the one in
// Nakama's runtime.env.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	udprelay "github.com/ikemen-engine/Ikemen-GO/nakama/udp-relay/relay"
)

func main() {
	listen := flag.String("listen", ":17360", "UDP address to listen on")
	secretFile := flag.String("secret-file", "", "file holding the shared secret (else $IKEMEN_UDP_RELAY_SECRET)")
	idle := flag.Duration("idle", 60*time.Second, "forget a side after this long without datagrams")
	pps := flag.Float64("pps", 500, "datagrams a second each side may send")
	bps := flag.Float64("bps", 256*1024, "bytes a second each side may send")
	maxSessions := flag.Int("max-sessions", 4096, "sessions held at once")
	statsEvery := flag.Duration("stats", 5*time.Minute, "log counters this often (0: never)")
	flag.Parse()

	secret := strings.TrimSpace(os.Getenv("IKEMEN_UDP_RELAY_SECRET"))
	if *secretFile != "" {
		b, err := os.ReadFile(*secretFile)
		if err != nil {
			log.Fatalf("udp-relay: %v", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	conn, err := listenUDP(*listen)
	if err != nil {
		log.Fatalf("udp-relay: %v", err)
	}
	srv, err := udprelay.New(conn, udprelay.Config{
		Secret:           []byte(secret),
		Idle:             *idle,
		PacketsPerSecond: *pps,
		BytesPerSecond:   *bps,
		MaxSessions:      *maxSessions,
	})
	if err != nil {
		log.Fatalf("udp-relay: %v (set -secret-file or IKEMEN_UDP_RELAY_SECRET)", err)
	}
	log.Printf("udp-relay: listening on %s", conn.LocalAddr())
	if *statsEvery > 0 {
		go func() {
			for range time.Tick(*statsEvery) {
				st := srv.Stats()
				log.Printf("udp-relay: sessions %d, joins %d, forwarded %d, dropped %d, refused %d",
					st.Sessions, st.Joins, st.Forwarded, st.Dropped, st.Refused)
			}
		}()
	}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		_ = conn.Close()
	}()
	if err := srv.Serve(); err != nil {
		log.Fatalf("udp-relay: %v", err)
	}
}

func listenUDP(addr string) (*net.UDPConn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen address %q: %w", addr, err)
	}
	return net.ListenUDP("udp", ua)
}
