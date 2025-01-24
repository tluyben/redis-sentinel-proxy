package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	password           string
	currentMaster      string
	mu                 sync.RWMutex
	sentinelErrorSent  bool
	masterErrorSent    bool
	activeConnections  int64    // Track number of active connections
	connectionHistory  []string // Track connection history
	connectionHistoryMu sync.Mutex
	maxConnections     int64    // Maximum allowed connections
	verbose           bool     // Verbose logging flag
}

func (p *RedisSentinelProxy) incrementConnections(clientAddr string) bool {
	newCount := atomic.AddInt64(&p.activeConnections, 1)
	
	// If we've exceeded max connections, decrement and return false
	if p.maxConnections > 0 && newCount > p.maxConnections {
		atomic.AddInt64(&p.activeConnections, -1)
		return false
	}
	
	p.connectionHistoryMu.Lock()
	p.connectionHistory = append(p.connectionHistory, fmt.Sprintf("%s - OPEN - Total: %d", clientAddr, newCount))
	p.connectionHistoryMu.Unlock()
	
	if p.verbose {
		log.Printf("New connection from %s (Total active: %d)", clientAddr, newCount)
	}
	return true
}

func (p *RedisSentinelProxy) decrementConnections(clientAddr string) {
	count := atomic.AddInt64(&p.activeConnections, -1)
	
	p.connectionHistoryMu.Lock()
	p.connectionHistory = append(p.connectionHistory, fmt.Sprintf("%s - CLOSE - Total: %d", clientAddr, count))
	// Keep only last 1000 connection events
	if len(p.connectionHistory) > 1000 {
		p.connectionHistory = p.connectionHistory[len(p.connectionHistory)-1000:]
	}
	p.connectionHistoryMu.Unlock()
	
	if p.verbose {
		log.Printf("Closed connection from %s (Total active: %d)", clientAddr, count)
	}
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

func (p *RedisSentinelProxy) reportSentinelError(err error) {
	if os.Getenv("SENTRY_DSN") == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.sentinelErrorSent {
		sentry.CaptureException(fmt.Errorf("failed to connect to sentinel (master name: %s, sentinels: %v): %v", p.masterName, p.sentinelAddrs, err))
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
		sentry.CaptureException(fmt.Errorf("failed to get master address for '%s' using sentinels %v: %v", p.masterName, p.sentinelAddrs, err))
		p.masterErrorSent = true
	}
}

func (p *RedisSentinelProxy) clearErrors() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sentinelErrorSent = false
	p.masterErrorSent = false
}

func readCommand(reader *bufio.Reader) (string, []string, error) {
	// Read the first line
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", nil, err
	}

	line = strings.TrimSpace(line)

	// Simple command format
	if !strings.HasPrefix(line, "*") {
		return strings.ToLower(line), nil, nil
	}

	// RESP array format
	count := 0
	_, err = fmt.Sscanf(line, "*%d", &count)
	if err != nil {
		return "", nil, err
	}

	var args []string
	for i := 0; i < count; i++ {
		// Read the $ line
		line, err = reader.ReadString('\n')
		if err != nil {
			return "", nil, err
		}

		// Read the actual argument
		line, err = reader.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		args = append(args, strings.TrimSpace(line))
	}

	if len(args) == 0 {
		return "", nil, fmt.Errorf("empty command")
	}

	return strings.ToLower(args[0]), args[1:], nil
}

func (p *RedisSentinelProxy) handleConnection(clientConn net.Conn, masterAddr string) {
	clientAddr := clientConn.RemoteAddr().String()
	
	// Check if we can accept new connection
	if !p.incrementConnections(clientAddr) {
		log.Printf("Rejected connection from %s: max connections reached", clientAddr)
		clientConn.Close()
		return
	}
	
	defer func() {
		clientConn.Close()
		p.decrementConnections(clientAddr)
	}()

	// Connect to master
	masterConn, err := net.Dial("tcp", masterAddr)
	if err != nil {
		wrappedErr := fmt.Errorf("failed to connect to master at %s from client %s (master name: %s): %w", masterAddr, clientAddr, p.masterName, err)
		log.Printf("%v", wrappedErr)
		p.reportMasterError(wrappedErr)
		return
	}
	defer masterConn.Close()

	// Create wait group to ensure both copies complete
	var wg sync.WaitGroup
	wg.Add(2)

	// Copy from client to master
	go func() {
		defer wg.Done()
		io.Copy(masterConn, clientConn)
		masterConn.(*net.TCPConn).CloseWrite()
	}()

	// Copy from master to client
	go func() {
		defer wg.Done()
		io.Copy(clientConn, masterConn)
		clientConn.(*net.TCPConn).CloseWrite()
	}()

	// Wait for both copies to complete
	wg.Wait()
}

func (p *RedisSentinelProxy) getConnectionStats() string {
	p.connectionHistoryMu.Lock()
	defer p.connectionHistoryMu.Unlock()
	
	current := atomic.LoadInt64(&p.activeConnections)
	history := strings.Join(p.connectionHistory[max(0, len(p.connectionHistory)-10):], "\n")
	
	return fmt.Sprintf("Current connections: %d\nRecent connection history:\n%s", 
		current, history)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (p *RedisSentinelProxy) Start(listenAddr string) error {
	// Initialize sentinel
	sntnl := &sentinel.Sentinel{
		Addrs:      p.sentinelAddrs,
		MasterName: p.masterName,
		Dial: func(addr string) (redis.Conn, error) {
			c, err := redis.Dial("tcp", addr)
			if err != nil {
				wrappedErr := fmt.Errorf("failed to connect to sentinel at %s (master name: %s, sentinels: %v): %w", addr, p.masterName, p.sentinelAddrs, err)
				log.Printf("%v", wrappedErr)
				p.reportSentinelError(wrappedErr)
				return nil, wrappedErr
			}

			if p.password != "" {
				if _, err := c.Do("AUTH", p.password); err != nil {
					c.Close()
					wrappedErr := fmt.Errorf("authentication failed for sentinel at %s (master name: %s, sentinels: %v): %w", addr, p.masterName, p.sentinelAddrs, err)
					log.Printf("%v", wrappedErr)
					p.reportSentinelError(wrappedErr)
					return nil, wrappedErr
				}
			}

			p.clearErrors()
			return c, nil
		},
	}

	// Get initial master
	masterAddr, err := sntnl.MasterAddr()
	if err != nil {
		wrappedErr := fmt.Errorf("initial master lookup failed for master '%s' using sentinels %v: %w", p.masterName, p.sentinelAddrs, err)
		p.reportMasterError(wrappedErr)
		return wrappedErr
	}

	// Add periodic connection logging
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		
		for range ticker.C {
			if p.verbose {
				current := atomic.LoadInt64(&p.activeConnections)
				log.Printf("Connection status - Active connections: %d", current)
			}
		}
	}()

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to start proxy listener on %s: %w", listenAddr, err)
	}

	log.Printf("Proxy listening on %s", listenAddr)
	log.Printf("Initial master: %s", masterAddr)

	// Update master in background
	go func() {
		for {
			newMasterAddr, err := sntnl.MasterAddr()
			if err != nil {
				wrappedErr := fmt.Errorf("failed to get master address for '%s' using sentinels %v: %w", p.masterName, p.sentinelAddrs, err)
				p.reportMasterError(wrappedErr)
				log.Printf("%v", wrappedErr)
				time.Sleep(time.Second)
				continue
			}

			p.clearErrors()

			if newMasterAddr != masterAddr {
				log.Printf("Master changed from %s to %s", masterAddr, newMasterAddr)
				masterAddr = newMasterAddr
			}

			time.Sleep(time.Second)
		}
	}()

	// Accept and handle connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting connection on %s: %v", listenAddr, err)
			continue
		}
		go p.handleConnection(conn, masterAddr)
	}
}

func main() {
	bindAddr := flag.String("bind", "0.0.0.0", "IP address to bind to")
	masterName := flag.String("service-name", "mymaster", "Service name")
	maxConns := flag.Int64("max-connections", 1000, "Maximum number of concurrent connections")
	verbose := flag.Bool("verbose", false, "Enable verbose connection logging")

	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		log.Fatalf("Usage: %s [-bind ip_address] [-max-connections n] [-verbose] server1,server2,server3", os.Args[0])
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
		sentinelAddrs:     sentinelAddrs,
		masterName:        *masterName,
		password:          password,
		maxConnections:    *maxConns,
		connectionHistory: make([]string, 0, 1000),
		verbose:           *verbose,
	}

	listenAddr := fmt.Sprintf("%s:%d", *bindAddr, PROXY_PORT)
	
	// Log startup configuration (excluding sensitive data)
	log.Printf("Starting Redis Sentinel Proxy with configuration:")
	log.Printf("- Bind Address: %s", listenAddr)
	log.Printf("- Master Name: %s", *masterName)
	log.Printf("- Sentinel Servers: %v", sentinelAddrs)
	log.Printf("- Max Connections: %d", *maxConns)
	log.Printf("- Verbose Logging: %v", *verbose)
	log.Printf("- Sentry Enabled: %v", os.Getenv("SENTRY_DSN") != "")

	log.Fatal(proxy.Start(listenAddr))
}
