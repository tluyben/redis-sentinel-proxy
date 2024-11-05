package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/FZambia/sentinel"
	"github.com/getsentry/sentry-go"
	"github.com/gomodule/redigo/redis"
	"github.com/joho/godotenv"
)

const (
	SENTINEL_PORT = 26379
	PROXY_PORT   = 6380
)

type RedisSentinelProxy struct {
	sentinelAddrs      []string
	masterName         string
	password          string
	currentMaster     string
	mu                sync.RWMutex
	sentinelErrorSent bool
	masterErrorSent   bool
}

func initSentry() {
	dsn := os.Getenv("SENTRY_DSN")
	if dsn == "" {
		log.Printf("No SENTRY_DSN found, error reporting disabled")
		return
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn: dsn,
	})
	if err != nil {
		log.Printf("Sentry initialization failed: %v", err)
	}
}

func loadEnv() string {
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found, using system environment variables")
	}

	password := os.Getenv("SENTINEL_PASSWORD")
	if password == "" {
		log.Printf("Warning: SENTINEL_PASSWORD not set")
	}

	return password
}

// func (p *RedisSentinelProxy) handleConnection(clientConn net.Conn, masterAddr string) {
// 	defer clientConn.Close()

// 	// Connect to master
// 	masterConn, err := net.Dial("tcp", masterAddr)
// 	if err != nil {
// 		log.Printf("Error connecting to master: %v", err)
// 		return
// 	}
// 	defer masterConn.Close()

// 	// Create bidirectional pipe
// 	go io.Copy(masterConn, clientConn)
// 	io.Copy(clientConn, masterConn)
// }

func (p *RedisSentinelProxy) reportSentinelError(err error) {
	if os.Getenv("SENTRY_DSN") == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.sentinelErrorSent {
		sentry.CaptureException(fmt.Errorf("cannot connect to sentinel: %v", err))
		p.sentinelErrorSent = true
	}
}

func (p *RedisSentinelProxy) reportMasterError(err error) {
	if os.Getenv("SENTRY_DSN") == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.masterErrorSent {
		sentry.CaptureException(fmt.Errorf("cannot get master: %v", err))
		p.masterErrorSent = true
	}
}

func (p *RedisSentinelProxy) clearErrors() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sentinelErrorSent = false
	p.masterErrorSent = false
}

func (p *RedisSentinelProxy) handleConnection(clientConn net.Conn, masterAddr string) {
	defer clientConn.Close()

	// Connect to master
	masterConn, err := net.DialTimeout("tcp", masterAddr, 5*time.Second)
	if err != nil {
		log.Printf("Error connecting to master: %v", err)
		return
	}
	defer masterConn.Close()

	// Use WaitGroup to ensure both goroutines complete
	var wg sync.WaitGroup
	wg.Add(2)

	// Channel to signal connection closure
	done := make(chan struct{})
	defer close(done)

	// Client -> Master
	go func() {
		defer wg.Done()
		buffer := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
				clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := clientConn.Read(buffer)
				if err != nil {
					if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
						log.Printf("Error reading from client: %v", err)
					}
					return
				}

				_, err = masterConn.Write(buffer[:n])
				if err != nil {
					log.Printf("Error writing to master: %v", err)
					return
				}
			}
		}
	}()

	// Master -> Client
	go func() {
		defer wg.Done()
		buffer := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
				masterConn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := masterConn.Read(buffer)
				if err != nil {
					if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
						log.Printf("Error reading from master: %v", err)
					}
					return
				}

				_, err = clientConn.Write(buffer[:n])
				if err != nil {
					log.Printf("Error writing to client: %v", err)
					return
				}
			}
		}
	}()

	// Wait for both goroutines to complete
	wg.Wait()
}

func (p *RedisSentinelProxy) Start(listenAddr string) error {
	// Initialize sentinel
	sntnl := &sentinel.Sentinel{
		Addrs:      p.sentinelAddrs,
		MasterName: p.masterName,
		Dial: func(addr string) (redis.Conn, error) {
			c, err := redis.Dial("tcp", addr,
				redis.DialReadTimeout(5*time.Second),
				redis.DialWriteTimeout(5*time.Second),
				redis.DialConnectTimeout(5*time.Second))
			if err != nil {
				p.reportSentinelError(err)
				return nil, err
			}

			if p.password != "" {
				if _, err := c.Do("AUTH", p.password); err != nil {
					c.Close()
					p.reportSentinelError(err)
					return nil, err
				}
			}

			p.clearErrors()
			return c, nil
		},
	}

	// Get initial master
	masterAddr, err := sntnl.MasterAddr()
	if err != nil {
		p.reportMasterError(err)
		return fmt.Errorf("initial master lookup failed: %v", err)
	}

	p.mu.Lock()
	p.currentMaster = masterAddr
	p.mu.Unlock()

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()

	log.Printf("Proxy listening on %s", listenAddr)
	log.Printf("Initial master: %s", masterAddr)

	// Monitor master changes
	go func() {
		for {
			time.Sleep(time.Second)
			newMasterAddr, err := sntnl.MasterAddr()
			if err != nil {
				p.reportMasterError(err)
				log.Printf("Error getting master address: %v", err)
				continue
			}

			p.mu.Lock()
			if newMasterAddr != p.currentMaster {
				log.Printf("Master changed from %s to %s", p.currentMaster, newMasterAddr)
				p.currentMaster = newMasterAddr
			}
			p.mu.Unlock()

			p.clearErrors()
		}
	}()

	// Accept connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Printf("Temporary error accepting connection: %v", err)
				time.Sleep(time.Second)
				continue
			}
			return err
		}

		go func(c net.Conn) {
			p.mu.RLock()
			currentMaster := p.currentMaster
			p.mu.RUnlock()
			p.handleConnection(c, currentMaster)
		}(conn)
	}
}

func main() {
	bindAddr := flag.String("bind", "0.0.0.0", "IP address to bind to")
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		log.Fatalf("Usage: %s [-bind ip_address] server1,server2,server3", os.Args[0])
	}

	servers := strings.Split(args[0], ",")
	if len(servers) == 0 {
		log.Fatal("No sentinel servers provided")
	}

	// Build sentinel addresses
	sentinelAddrs := make([]string, len(servers))
	for i, server := range servers {
		sentinelAddrs[i] = fmt.Sprintf("%s:%d", strings.TrimSpace(server), SENTINEL_PORT)
	}

	password := loadEnv()
	initSentry() // Initialize Sentry if DSN is available

	proxy := &RedisSentinelProxy{
		sentinelAddrs: sentinelAddrs,
		masterName:    "mymaster",
		password:      password,
	}

	listenAddr := fmt.Sprintf("%s:%d", *bindAddr, PROXY_PORT)
	log.Fatal(proxy.Start(listenAddr))
}
