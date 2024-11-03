# 🚀 Redis Sentinel Proxy

A lightweight proxy that sits between your Redis clients and Redis Sentinel setup. It makes your sentinel-managed Redis cluster appear as a single Redis instance to clients.

## 🌟 Features

- 🔄 Automatic master failover handling
- 🔐 Password authentication support
- 🔌 Standard Redis protocol compatibility
- 🚪 Custom port configuration
- 🌐 Configurable bind address
- 📝 Environment variable support

## 🏗️ Installation

```bash
# Clone the repository
git clone https://github.com/tluyben/redis-sentinel-proxy
cd redis-sentinel-proxy

# Install dependencies
go mod init redis-sentinel-proxy
go get github.com/gomodule/redigo/redis
go get github.com/FZambia/sentinel
go get github.com/joho/godotenv

# Build the binary
go build -o redis-sentinel-proxy
```

## ⚙️ Configuration

1. Create a `.env` file in the same directory as the binary:

```bash
SENTINEL_PASSWORD=your_sentinel_password_here
```

Or set the environment variable directly:

```bash
export SENTINEL_PASSWORD=your_sentinel_password_here
```

## 🚀 Usage

Basic usage with default settings (binds to 0.0.0.0):
```bash
./redis-sentinel-proxy server1,server2,server3
```

To bind to a specific IP address:
```bash
./redis-sentinel-proxy -bind 127.0.0.1 server1,server2,server3
```

For example:

```bash
# Bind to all interfaces (default)
./redis-sentinel-proxy redis-sentinel-1.example.com,redis-sentinel-2.example.com,redis-sentinel-3.example.com

# Bind to localhost only
./redis-sentinel-proxy -bind 127.0.0.1 redis-sentinel-1.example.com,redis-sentinel-2.example.com,redis-sentinel-3.example.com
```

## 📌 Port Configuration

- Redis Sentinel ports are fixed at `26379`
- Proxy listens on port `6380`

## 🔍 Testing

You can test the connection using any Redis client:

```bash
# Using redis-cli
redis-cli -p 6380

# Using telnet
telnet localhost 6380
AUTH yourpassword
SET mykey "Hello World"
GET mykey
```

## 🏗️ Architecture

```
Client -> Redis Sentinel Proxy (6380) -> Sentinel (26379) -> Redis Master
```

The proxy:

1. 🔍 Uses Sentinel to discover the current master
2. 🔄 Continuously monitors for master changes
3. 📡 Proxies all Redis commands to the current master

## ⚠️ Notes

- Ensure your Redis Sentinel setup is properly configured with authentication
- The proxy needs to be able to reach all sentinel servers
- Client applications connect to the proxy as if it were a regular Redis instance
- By default, the proxy binds to all interfaces (0.0.0.0). Use the -bind flag to restrict to specific interfaces

## 🐛 Troubleshooting

If you see connection errors:

1. Verify sentinel addresses are correct
2. Check if `SENTINEL_PASSWORD` is set correctly
3. Ensure sentinel servers are reachable
4. Check firewall rules for ports 26379 and 6380
5. Verify the bind address is accessible from your client

## 🤝 Contributing

Pull requests are welcome! For major changes, please open an issue first to discuss what you would like to change.

## 📜 License

MIT

---

Made with ❤️ by @luyben
