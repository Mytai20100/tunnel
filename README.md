# Tunnel

<div align="center">

![Version](https://img.shields.io/badge/version-3.3-blue.svg)
![Language](https://img.shields.io/badge/language-Go-00ADD8.svg)
![Stars](https://img.shields.io/github/stars/Mytai20100/tunnel?style=social)
![Views](https://img.shields.io/github/watchers/Mytai20100/tunnel?style=social)
![Pulls](https://img.shields.io/github/downloads/Mytai20100/tunnel/total)

**A high-performance tunnel proxy with real-time monitoring and analytics**

[Features](#features) • [Installation](#installation) • [Usage](#usage) • [API](#api) • [Configuration](#configuration)

</div>

---

## Features

- **High Performance**: Handles thousands of concurrent miner connections
- **Real-time Monitoring**: Live hashrate, shares, and network statistics
- **TLS Support**: Secure connections with optional TLS encryption
- **SQLite Database**: Efficient data storage with automatic cleanup
- **REST API**: Full-featured API for integration
- **Prometheus Metrics**: Built-in metrics endpoint for monitoring
- **WebSocket Logs**: Real-time log streaming
- **Multi-Pool Support**: Connect to multiple mining pools simultaneously

---

## Installation

### Prerequisites

Before installing Tunnel, ensure you have the following:

- **Go 1.25.5** - [Download Go](https://golang.org/dl/)
- **Git** - [Download Git](https://git-scm.com/downloads)
- **gcc** (for CGO) - Required for some dependencies

#### Install Go on Linux/Ubuntu:
```bash
wget https://go.dev/dl/go1.21.5.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version
```

#### Install Go on Windows:
Download and run the installer from [golang.org/dl](https://golang.org/dl/)

---

## 📦 Building from Source

### 1. Clone the Repository
```bash
git clone https://github.com/Mytai20100/tunnel.git
cd tunnel
```

### 2. Install Dependencies
```bash
go mod download
go mod tidy
```

### 3. Build the Binary

**Linux/macOS:**
```bash
go build -o tunnel main.go
```

**Windows:**
```bash
go build -o tunnel.exe main.go
```
---

## Configuration

### Create `config.yml`

On first run, Tunnel will create a default `config.yml` file. Edit it to match your setup:
```yaml
pools:
  pool1:
    host: "pool.example.com"
    port: 4444
    name: "Example Pool"
  
  pool2:
    host: "another-pool.com"
    port: 3333
    name: "Another Pool"

tunnels:
  tunnel1:
    ip: "0.0.0.0"
    port: 3333
    pool: "pool1"
  
  tunnel2:
    ip: "0.0.0.0"
    port: 3334
    pool: "pool2"

api_port: 8080

database:
  host: "localhost"
  port: 3306
  user: "root"
  password: "password"
  dbname: "mining_tunnel"
```

---

## Usage

### Basic Usage
```bash
# Run with default settings
./tunnel

# Show help
./tunnel --help

# Show version
./tunnel --version
```

### Command Line Options

| Option | Description |
|--------|-------------|
| `--nodata` | Disable database logging |
| `--noapi` | Disable API server |
| `--nodebug` | Minimal output mode (single line status) |
| `--tls` | Enable TLS support for miner connections |
| `--tlscert` | TLS certificate file (default: cert.pem) |
| `--tlskey` | TLS key file (default: key.pem) |
| `--help` | Show help message |
| `--version` | Show version |

### Examples
```bash
# Run without database
./tunnel --nodata

# Run with minimal output
./tunnel --nodebug

# Run with TLS support
./tunnel --tls --tlscert=/path/to/cert.pem --tlskey=/path/to/key.pem

# Run without database and API
./tunnel --nodata --noapi
```

---

## TLS Setup (Optional)

To enable TLS encryption, generate certificates:

### Generate Self-Signed Certificate:
```bash
openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem -days 365 -nodes
```

### Run with TLS:
```bash
./tunnel --tls --tlscert=cert.pem --tlskey=key.pem
```

---

## API Endpoints

### System Metrics
```bash
GET http://localhost:8080/api/metrics
```

### Miner Information
```bash
GET http://localhost:8080/api/i/{wallet_address}
```

### Network Statistics
```bash
GET http://localhost:8080/api/network/stats?hours=24
```

### Shares Statistics
```bash
GET http://localhost:8080/api/shares/stats?wallet={address}&hours=24
```

### Prometheus Metrics
```bash
GET http://localhost:8080/metrics
```

### WebSocket Log Stream
```bash
WS ws://localhost:8080/api/logs/stream
```

---

## Example API Response

**GET /api/metrics:**
```json
{
  "system": {
    "cpu_model": "Intel Core i7",
    "cpu_cores": 8,
    "cpu_usage_percent": "25.50%",
    "ram_total_bytes": 17179869184,
    "ram_used_bytes": 8589934592,
    "uptime_seconds": 3600
  },
  "network": {
    "total_download_gb": 1.25,
    "total_upload_gb": 0.85,
    "packets_sent": 150000,
    "packets_received": 145000
  },
  "miners": {
    "active_count": 5,
    "list": [...]
  }
}
```

---

## Docker Support (Optional)

### Dockerfile
```dockerfile
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY . .
RUN go mod download
RUN go build -o tunnel main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/tunnel .
COPY config.yml .

EXPOSE 3333 8080
CMD ["./tunnel"]
```

### Build & Run:
```bash
docker build -t tunnel:latest .
docker run -d -p 3333:3333 -p 8080:8080 --name tunnel tunnel:latest
```

---

## 🛠️ Systemd Service (Linux)

Create `/etc/systemd/system/tunnel.service`:
```ini
[Unit]
Description=Tunnel Mining Proxy
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/root/tunnel
ExecStart=/root/tunnel/tunnel
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

### Enable & Start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable tunnel
sudo systemctl start tunnel
sudo systemctl status tunnel
```

---

## Logs & Debugging

### View Logs:
```bash
# log for tunnel
tail -f /var/log/tunnel.log

# Systemd logs
journalctl -u tunnel -f
```

---

## 🤝 Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/AmazingFeature`)
3. Commit your changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

---

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

---

## 🙏 Acknowledgments

- Built with [Go](https://golang.org/)
- Uses [Gorilla WebSocket](https://github.com/gorilla/websocket)
- Database: [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)
- System metrics: [gopsutil](https://github.com/shirou/gopsutil)

---

<div align="center">

**Made with ❤️ by [mytai](https://github.com/Mytai20100)**

[GitHub Repository](https://github.com/Mytai20100/tunnel/tree/main)

⭐ **Star this repo if you find it helpful!** ⭐

</div>
