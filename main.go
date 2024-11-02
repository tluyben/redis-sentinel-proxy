package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/FZambia/sentinel"
	"github.com/gomodule/redigo/redis"
	"github.com/joho/godotenv"
)

const (
    SENTINEL_PORT = 26379
    PROXY_PORT   = 6380
)

type RedisSentinelProxy struct {
    sentinelAddrs []string
    masterName    string
    password      string
    currentMaster string
    mu            sync.RWMutex
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

func (p *RedisSentinelProxy) handleConnection(clientConn net.Conn, masterAddr string) {
    defer clientConn.Close()

    // Connect to master
    masterConn, err := net.Dial("tcp", masterAddr)
    if err != nil {
        log.Printf("Error connecting to master: %v", err)
        return
    }
    defer masterConn.Close()

    // Create bidirectional pipe
    go io.Copy(masterConn, clientConn)
    io.Copy(clientConn, masterConn)
}

func (p *RedisSentinelProxy) Start(listenAddr string) error {
    // Initialize sentinel
    sntnl := &sentinel.Sentinel{
        Addrs:      p.sentinelAddrs,
        MasterName: p.masterName,
        Dial: func(addr string) (redis.Conn, error) {
            c, err := redis.Dial("tcp", addr)
            if err != nil {
                return nil, err
            }
            
            if p.password != "" {
                if _, err := c.Do("AUTH", p.password); err != nil {
                    c.Close()
                    return nil, err
                }
            }
            
            return c, nil
        },
    }

    // Get initial master
    masterAddr, err := sntnl.MasterAddr()
    if err != nil {
        return fmt.Errorf("initial master lookup failed: %v", err)
    }

    listener, err := net.Listen("tcp", listenAddr)
    if err != nil {
        return err
    }
    
    log.Printf("Proxy listening on %s", listenAddr)
    log.Printf("Initial master: %s", masterAddr)
    
    // Update master in background
    go func() {
        for {
            newMasterAddr, err := sntnl.MasterAddr()
            if err != nil {
                log.Printf("Error getting master address: %v", err)
                time.Sleep(time.Second)
                continue
            }
            
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
            log.Printf("Error accepting connection: %v", err)
            continue
        }
        go p.handleConnection(conn, masterAddr)
    }
}

func main() {
    if len(os.Args) != 2 {
        log.Fatalf("Usage: %s server1,server2,server3", os.Args[0])
    }
    
    servers := strings.Split(os.Args[1], ",")
    if len(servers) == 0 {
        log.Fatal("No sentinel servers provided")
    }
    
    // Build sentinel addresses
    sentinelAddrs := make([]string, len(servers))
    for i, server := range servers {
        sentinelAddrs[i] = fmt.Sprintf("%s:%d", strings.TrimSpace(server), SENTINEL_PORT)
    }
    
    password := loadEnv()
    
    proxy := &RedisSentinelProxy{
        sentinelAddrs: sentinelAddrs,
        masterName:    "mymaster",
        password:      password,
    }
    
    listenAddr := fmt.Sprintf(":%d", PROXY_PORT)
    log.Fatal(proxy.Start(listenAddr))
}