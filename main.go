package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"

	"sync"
	"time"

	"gopkg.in/yaml.v2"
)

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorPurple = "\033[35m"
	ColorCyan   = "\033[36m"
	ColorWhite  = "\033[37m"
)

type PoolConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	Name string `yaml:"name"`
}

type TunnelConfig struct {
	IP   string `yaml:"ip"`
	Port int    `yaml:"port"`
	Pool string `yaml:"pool"`
}

type Config struct {
	Pools   map[string]PoolConfig   `yaml:"pools"`
	Tunnels map[string]TunnelConfig `yaml:"tunnels"`
}

type StratumMessage struct {
	ID     interface{} `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
	Result interface{} `json:"result"`
	Error  interface{} `json:"error"`
}

type MinerInfo struct {
	Name     string
	IP       string
	Port     string
	JobID    string
	Shares   int
	LastSeen time.Time
}

var (
	config      Config
	miners      = make(map[string]*MinerInfo)
	minersMutex sync.RWMutex
)

func main() {
	fmt.Printf("Tunnelv3,2 by mytai =) \n")
	loadConfig()
	var wg sync.WaitGroup
	for tunnelName, tunnelConf := range config.Tunnels {
		wg.Add(1)
		go func(name string, conf TunnelConfig) {
			defer wg.Done()
			startTunnel(name, conf)
		}(tunnelName, tunnelConf)
	}
	fmt.Printf("%s[INFO]%s Loaded %d pools\n", ColorGreen, ColorReset, len(config.Pools))
	fmt.Printf("%s[INFO]%s Tunnel Started\n", ColorGreen, ColorReset)
	fmt.Printf("%s[INFO]%s Active tunnels: %d\n", ColorGreen, ColorReset, len(config.Tunnels))

	wg.Wait()
}

func loadConfig() {
	configFile := "config.yml"

	// Check if config file exists
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		// Create default config
		config = Config{
			Pools: map[string]PoolConfig{
				"pool1": {
					Host: "pool.example.com",
					Port: 4444,
					Name: "Example Pool",
				},
			},
			Tunnels: map[string]TunnelConfig{
				"tunnel1": {
					IP:   "0.0.0.0",
					Port: 3333,
					Pool: "pool1",
				},
			},
		}
		saveConfig()
		fmt.Printf("%s[INFO]%s Created default config.yml\n", ColorYellow, ColorReset)
	} else {
		// Load existing config
		data, err := os.ReadFile(configFile)
		if err != nil {
			log.Fatalf("Error reading config: %v", err)
		}

		err = yaml.Unmarshal(data, &config)
		if err != nil {
			log.Fatalf("Error parsing config: %v", err)
		}
		fmt.Printf("%s[INFO]%s Loaded config.yml\n", ColorGreen, ColorReset)
	}
}

func saveConfig() {
	data, err := yaml.Marshal(&config)
	if err != nil {
		log.Printf("Error marshaling config: %v", err)
		return
	}

	err = os.WriteFile("config.yml", data, 0644)
	if err != nil {
		log.Printf("Error writing config: %v", err)
	}
}

func startTunnel(tunnelName string, tunnelConf TunnelConfig) {
	poolConf, exists := config.Pools[tunnelConf.Pool]
	if !exists {
		fmt.Printf("%s[ERR]%s Tunnel %s: Pool %s not found in config\n",
			ColorRed, ColorReset, tunnelName, tunnelConf.Pool)
		return
	}

	addr := fmt.Sprintf("%s:%d", tunnelConf.IP, tunnelConf.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("%s[ERR]%s Tunnel %s: Failed to listen on %s: %v\n",
			ColorRed, ColorReset, tunnelName, addr, err)
		return
	}
	defer listener.Close()

	fmt.Printf("%s[INFO]%s Tunnel %s listening on %s -> %s:%d (%s)\n",
		ColorCyan, ColorReset, tunnelName, addr, poolConf.Host, poolConf.Port, poolConf.Name)

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			fmt.Printf("%s[ERR]%s Tunnel %s: Accept error: %v\n",
				ColorRed, ColorReset, tunnelName, err)
			continue
		}

		go handleConnection(tunnelName, clientConn, poolConf)
	}
}

func handleConnection(tunnelName string, clientConn net.Conn, poolConf PoolConfig) {
	defer clientConn.Close()

	clientAddr := clientConn.RemoteAddr().String()
	clientIP, clientPort, _ := net.SplitHostPort(clientAddr)

	fmt.Printf("%s[INFO]%s %s New connection from %s:%s\n",
		ColorBlue, ColorReset, getCurrentTime(), clientIP, clientPort)

	// Connect to pool
	poolAddr := fmt.Sprintf("%s:%d", poolConf.Host, poolConf.Port)
	poolConn, err := net.Dial("tcp", poolAddr)
	if err != nil {
		fmt.Printf("%s[ERR]%s %s Failed to connect to pool %s: %v\n",
			ColorRed, ColorReset, getCurrentTime(), poolAddr, err)
		return
	}
	defer poolConn.Close()

	fmt.Printf("%s[INFO]%s %s Connected to pool %s (%s)\n",
		ColorGreen, ColorReset, getCurrentTime(), poolAddr, poolConf.Name)

	// Create miner info
	minerKey := fmt.Sprintf("%s:%s", clientIP, clientPort)
	minersMutex.Lock()
	miners[minerKey] = &MinerInfo{
		Name:     "Unknown",
		IP:       clientIP,
		Port:     clientPort,
		JobID:    "",
		Shares:   0,
		LastSeen: time.Now(),
	}
	minersMutex.Unlock()

	// Start proxying
	done := make(chan bool, 2)

	// Client -> Pool
	go func() {
		defer func() { done <- true }()
		proxyData(clientConn, poolConn, tunnelName, poolConf, minerKey, true)
	}()

	// Pool -> Client
	go func() {
		defer func() { done <- true }()
		proxyData(poolConn, clientConn, tunnelName, poolConf, minerKey, false)
	}()

	// Wait for either direction to close
	<-done
	fmt.Printf("%s[INFO]%s %s Connection closed for %s:%s\n",
		ColorYellow, ColorReset, getCurrentTime(), clientIP, clientPort)

	// Clean up miner info
	minersMutex.Lock()
	delete(miners, minerKey)
	minersMutex.Unlock()
}

func proxyData(src, dst net.Conn, tunnelName string, poolConf PoolConfig, minerKey string, clientToPool bool) {
	scanner := bufio.NewScanner(src)
	for scanner.Scan() {
		line := scanner.Text()

		// Parse and log stratum messages
		if clientToPool {
			parseClientMessage(line, tunnelName, poolConf, minerKey)
		} else {
			parsePoolMessage(line, tunnelName, poolConf, minerKey)
		}

		// Forward the message
		_, err := fmt.Fprintf(dst, "%s\n", line)
		if err != nil {
			break
		}
	}
}

func parseClientMessage(message, tunnelName string, poolConf PoolConfig, minerKey string) {
	var msg StratumMessage
	if err := json.Unmarshal([]byte(message), &msg); err != nil {
		return
	}

	minersMutex.Lock()
	miner, exists := miners[minerKey]
	if !exists {
		minersMutex.Unlock()
		return
	}

	switch msg.Method {
	case "mining.authorize":
		if params, ok := msg.Params.([]interface{}); ok && len(params) > 0 {
			if username, ok := params[0].(string); ok {
				miner.Name = username
				fmt.Printf("%s[INFO]%s %s Miner %s (%s:%s) authorized on tunnel %s -> %s\n",
					ColorGreen, ColorReset, getCurrentTime(), username, miner.IP, miner.Port, tunnelName, poolConf.Name)
			}
		}

	case "mining.submit":
		if params, ok := msg.Params.([]interface{}); ok && len(params) >= 3 {
			miner.Shares++
			jobID := ""
			if jid, ok := params[1].(string); ok {
				jobID = jid
				miner.JobID = jobID
			}

			fmt.Printf("%s[SHARE]%s %s Miner %s (%s:%s) job_id=%s * share %d * pool %s port %d\n",
				ColorPurple, ColorReset, getCurrentTime(), miner.Name, miner.IP, miner.Port,
				jobID, miner.Shares, poolConf.Name, poolConf.Port)
		}
	}

	miner.LastSeen = time.Now()
	minersMutex.Unlock()
}

func parsePoolMessage(message, tunnelName string, poolConf PoolConfig, minerKey string) {
	var msg StratumMessage
	if err := json.Unmarshal([]byte(message), &msg); err != nil {
		return
	}

	minersMutex.Lock()
	miner, exists := miners[minerKey]
	if !exists {
		minersMutex.Unlock()
		return
	}

	// Check for errors or rejections
	if msg.Error != nil {
		fmt.Printf("%s[ERR]%s %s Miner %s (%s:%s) * Error from pool %s: %v\n",
			ColorRed, ColorReset, getCurrentTime(), miner.Name, miner.IP, miner.Port,
			poolConf.Name, msg.Error)
	}

	// Check for job notifications
	if msg.Method == "mining.notify" {
		if params, ok := msg.Params.([]interface{}); ok && len(params) > 0 {
			if jobID, ok := params[0].(string); ok {
				miner.JobID = jobID
				fmt.Printf("%s[INFO]%s %s New job %s for miner %s (%s:%s) from pool %s\n",
					ColorCyan, ColorReset, getCurrentTime(), jobID, miner.Name, miner.IP, miner.Port, poolConf.Name)
			}
		}
	}

	// Check for submit responses
	if msg.ID != nil && msg.Result != nil {
		if result, ok := msg.Result.(bool); ok {
			if result {
				fmt.Printf("%s[SHARE]%s %s Share ACCEPTED for miner %s (%s:%s) from pool %s\n",
					ColorGreen, ColorReset, getCurrentTime(), miner.Name, miner.IP, miner.Port, poolConf.Name)
			} else {
				fmt.Printf("%s[SHARE]%s %s Share REJECTED for miner %s (%s:%s) from pool %s\n",
					ColorRed, ColorReset, getCurrentTime(), miner.Name, miner.IP, miner.Port, poolConf.Name)
			}
		}
	}

	miner.LastSeen = time.Now()
	minersMutex.Unlock()
}

func getCurrentTime() string {
	return time.Now().Format("2006-01-02 15:04:05")
}
