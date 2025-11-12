package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"gopkg.in/yaml.v2"
)

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
	Pools    map[string]PoolConfig   `yaml:"pools"`
	Tunnels  map[string]TunnelConfig `yaml:"tunnels"`
	APIPort  int                     `yaml:"api_port"`
	Database DatabaseConfig          `yaml:"database"`
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
}

type StratumMessage struct {
	ID     interface{} `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
	Result interface{} `json:"result"`
	Error  interface{} `json:"error"`
}

type MinerInfo struct {
	Wallet          string
	Name            string
	IP              string
	Port            string
	PoolName        string
	JobID           string
	SharesAccepted  int
	SharesRejected  int
	LastSeen        time.Time
	ConnectedAt     time.Time
	BytesDownload   int64
	BytesUpload     int64
	PacketsSent     int64
	PacketsReceived int64
}

type PoolMetrics struct {
	Name            string
	CurrentPing     float64
	AveragePing     float64
	PingSamples     []float64
	AvgAcceptTime   float64
	AcceptTimes     []float64
	SharesAccepted  int64
	SharesRejected  int64
	LastPingTime    time.Time
}

type SystemMetrics struct {
	CPUModel        string
	CPUCores        int
	RAMTotal        uint64
	RAMUsed         uint64
	DiskTotal       uint64
	DiskUsed        uint64
	OS              string
	PublicIP        string
	Uptime          time.Duration
	TotalDownload   int64
	TotalUpload     int64
	PacketsSent     int64
	PacketsReceived int64
	ActiveMiners    int
}

type LogMessage struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Color     string `json:"color"`
}

var (
	config         Config
	db             *sql.DB
	miners         = make(map[string]*MinerInfo)
	minersMutex    sync.RWMutex
	poolMetrics    = make(map[string]*PoolMetrics)
	poolMetricsMtx sync.RWMutex
	systemMetrics  SystemMetrics
	systemMtx      sync.RWMutex
	startTime      time.Time
	logClients     = make(map[*websocket.Conn]bool)
	logClientsMtx  sync.RWMutex
	logBroadcast   = make(chan LogMessage, 100)
	upgrader       = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func main() {
	fmt.Printf("Tunnelv3.2 by mytai\n%s\n", strings.Repeat("-", 60))
	startTime = time.Now()
	loadConfig()
	initDatabase()
	initSystemMetrics()
	var wg sync.WaitGroup
	for tunnelName, tunnelConf := range config.Tunnels {
		wg.Add(1)
		go func(name string, conf TunnelConfig) {
			defer wg.Done()
			startTunnel(name, conf)
		}(tunnelName, tunnelConf)
	}
	go startAPIServer()
	go monitorPoolPings()
	go updateSystemMetrics()
	LogInfo("Loaded %d pools", len(config.Pools))
	LogInfo("Tunnel Started")
	LogInfo("Active tunnels: %d", len(config.Tunnels))
	LogInfo("API server running on port %d", config.APIPort)
	fmt.Printf("%s\n", strings.Repeat("-", 60))
	wg.Wait()
}

func loadConfig() {
	configFile := "config.yml"
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		config = Config{
			Pools: map[string]PoolConfig{
				"pool1": {Host: "pool.example.com", Port: 4444, Name: "Example Pool"},
			},
			Tunnels: map[string]TunnelConfig{
				"tunnel1": {IP: "0.0.0.0", Port: 3333, Pool: "pool1"},
			},
			APIPort: 8080,
			Database: DatabaseConfig{
				Host: "localhost", Port: 3306, User: "root",
				Password: "password", DBName: "mining_tunnel",
			},
		}
		saveConfig()
		LogWarning("Created default config.yml")
	} else {
		data, err := os.ReadFile(configFile)
		if err != nil {
			log.Fatalf("Error reading config: %v", err)
		}
		err = yaml.Unmarshal(data, &config)
		if err != nil {
			log.Fatalf("Error parsing config: %v", err)
		}
		LogInfo("Loaded config.yml")
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

func initDatabase() {
	var err error
	db, err = sql.Open("sqlite", "./data.db")
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}
	createTables()
	LogInfo("Database connected (SQLite - Pure Go)")
}

func createTables() {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS miners (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			wallet TEXT NOT NULL, miner_name TEXT, ip TEXT, pool_name TEXT,
			shares_accepted INTEGER DEFAULT 0, shares_rejected INTEGER DEFAULT 0,
			bytes_download INTEGER DEFAULT 0, bytes_upload INTEGER DEFAULT 0,
			packets_sent INTEGER DEFAULT 0, packets_received INTEGER DEFAULT 0,
			connected_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(wallet, ip, miner_name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_wallet ON miners(wallet)`,
		`CREATE INDEX IF NOT EXISTS idx_ip ON miners(ip)`,
		`CREATE TABLE IF NOT EXISTS shares (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			wallet TEXT NOT NULL, miner_name TEXT, ip TEXT,
			pool_name TEXT, job_id TEXT, accepted INTEGER,
			submitted_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_wallet_shares ON shares(wallet)`,
		`CREATE INDEX IF NOT EXISTS idx_submitted_at ON shares(submitted_at)`,
	}
	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			log.Fatalf("Failed to create table: %v", err)
		}
	}
}

func saveMinerToDB(miner *MinerInfo) {
	checkQuery := `SELECT id, shares_accepted, shares_rejected, bytes_download, bytes_upload, 
		packets_sent, packets_received FROM miners 
		WHERE wallet = ? AND ip = ? AND miner_name = ?`
	var existingID int
	var existingAccepted, existingRejected int
	var existingDownload, existingUpload, existingSent, existingReceived int64
	err := db.QueryRow(checkQuery, miner.Wallet, miner.IP, miner.Name).Scan(
		&existingID, &existingAccepted, &existingRejected,
		&existingDownload, &existingUpload, &existingSent, &existingReceived)
	if err == sql.ErrNoRows {
		insertQuery := `INSERT INTO miners (wallet, miner_name, ip, pool_name, shares_accepted, 
			shares_rejected, bytes_download, bytes_upload, packets_sent, packets_received, 
			connected_at, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		_, err = db.Exec(insertQuery, miner.Wallet, miner.Name, miner.IP, miner.PoolName,
			miner.SharesAccepted, miner.SharesRejected, miner.BytesDownload, miner.BytesUpload,
			miner.PacketsSent, miner.PacketsReceived, miner.ConnectedAt, miner.LastSeen)
	} else if err == nil {
		updateQuery := `UPDATE miners SET shares_accepted = ?, shares_rejected = ?,
			bytes_download = ?, bytes_upload = ?, packets_sent = ?, packets_received = ?,
			last_seen = ?, pool_name = ? WHERE id = ?`
		_, err = db.Exec(updateQuery,
			existingAccepted+miner.SharesAccepted, existingRejected+miner.SharesRejected,
			existingDownload+miner.BytesDownload, existingUpload+miner.BytesUpload,
			existingSent+miner.PacketsSent, existingReceived+miner.PacketsReceived,
			miner.LastSeen, miner.PoolName, existingID)
	}
	if err != nil {
		log.Printf("Failed to save miner to DB: %v", err)
	}
}

func saveShareToDB(wallet, minerName, ip, poolName, jobID string, accepted bool) {
	acceptedInt := 0
	if accepted {
		acceptedInt = 1
	}
	query := `INSERT INTO shares (wallet, miner_name, ip, pool_name, job_id, accepted)
		VALUES (?, ?, ?, ?, ?, ?)`
	_, err := db.Exec(query, wallet, minerName, ip, poolName, jobID, acceptedInt)
	if err != nil {
		log.Printf("Failed to save share to DB: %v", err)
	}
}

func initSystemMetrics() {
	systemMtx.Lock()
	defer systemMtx.Unlock()
	cpuInfo, _ := cpu.Info()
	if len(cpuInfo) > 0 {
		systemMetrics.CPUModel = cpuInfo[0].ModelName
		systemMetrics.CPUCores = int(cpuInfo[0].Cores)
	}
	hostInfo, _ := host.Info()
	if hostInfo != nil {
		systemMetrics.OS = hostInfo.OS + " " + hostInfo.PlatformVersion
	}
	memInfo, _ := mem.VirtualMemory()
	if memInfo != nil {
		systemMetrics.RAMTotal = memInfo.Total
	}
	diskInfo, _ := disk.Usage("/")
	if diskInfo != nil {
		systemMetrics.DiskTotal = diskInfo.Total
	}
	go func() {
		resp, err := http.Get("https://api.ipify.org?format=text")
		if err == nil {
			defer resp.Body.Close()
			buf := make([]byte, 128)
			n, _ := resp.Body.Read(buf)
			systemMtx.Lock()
			systemMetrics.PublicIP = string(buf[:n])
			systemMtx.Unlock()
		}
	}()
}

func updateSystemMetrics() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		systemMtx.Lock()
		memInfo, _ := mem.VirtualMemory()
		if memInfo != nil {
			systemMetrics.RAMUsed = memInfo.Used
		}
		diskInfo, _ := disk.Usage("/")
		if diskInfo != nil {
			systemMetrics.DiskUsed = diskInfo.Used
		}
		systemMetrics.Uptime = time.Since(startTime)
		minersMutex.RLock()
		systemMetrics.ActiveMiners = len(miners)
		systemMetrics.TotalDownload = 0
		systemMetrics.TotalUpload = 0
		systemMetrics.PacketsSent = 0
		systemMetrics.PacketsReceived = 0
		for _, miner := range miners {
			systemMetrics.TotalDownload += miner.BytesDownload
			systemMetrics.TotalUpload += miner.BytesUpload
			systemMetrics.PacketsSent += miner.PacketsSent
			systemMetrics.PacketsReceived += miner.PacketsReceived
		}
		minersMutex.RUnlock()
		systemMtx.Unlock()
	}
}

func monitorPoolPings() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for poolName, poolConf := range config.Pools {
			go measurePoolPing(poolName, poolConf)
		}
	}
}

func measurePoolPing(poolName string, poolConf PoolConfig) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", poolConf.Host, poolConf.Port), 5*time.Second)
	if err != nil {
		return
	}
	conn.Close()
	pingTime := time.Since(start).Seconds() * 1000
	poolMetricsMtx.Lock()
	defer poolMetricsMtx.Unlock()
	if poolMetrics[poolName] == nil {
		poolMetrics[poolName] = &PoolMetrics{
			Name: poolName, PingSamples: make([]float64, 0), AcceptTimes: make([]float64, 0),
		}
	}
	pm := poolMetrics[poolName]
	pm.CurrentPing = pingTime
	pm.PingSamples = append(pm.PingSamples, pingTime)
	pm.LastPingTime = time.Now()
	if len(pm.PingSamples) > 100 {
		pm.PingSamples = pm.PingSamples[1:]
	}
	sum := 0.0
	for _, p := range pm.PingSamples {
		sum += p
	}
	pm.AveragePing = sum / float64(len(pm.PingSamples))
}

func recordAcceptTime(poolName string, duration float64) {
	poolMetricsMtx.Lock()
	defer poolMetricsMtx.Unlock()
	if poolMetrics[poolName] == nil {
		poolMetrics[poolName] = &PoolMetrics{
			Name: poolName, PingSamples: make([]float64, 0), AcceptTimes: make([]float64, 0),
		}
	}
	pm := poolMetrics[poolName]
	pm.AcceptTimes = append(pm.AcceptTimes, duration)
	if len(pm.AcceptTimes) > 100 {
		pm.AcceptTimes = pm.AcceptTimes[1:]
	}
	sum := 0.0
	for _, t := range pm.AcceptTimes {
		sum += t
	}
	pm.AvgAcceptTime = sum / float64(len(pm.AcceptTimes))
}

func startTunnel(tunnelName string, tunnelConf TunnelConfig) {
	poolConf, exists := config.Pools[tunnelConf.Pool]
	if !exists {
		LogError("Tunnel %s: Pool %s not found in config", tunnelName, tunnelConf.Pool)
		return
	}
	addr := fmt.Sprintf("%s:%d", tunnelConf.IP, tunnelConf.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		LogError("Tunnel %s: Failed to listen on %s: %v", tunnelName, addr, err)
		return
	}
	defer listener.Close()
	LogDebug("Tunnel %s listening on %s -> %s:%d (%s)", tunnelName, addr, poolConf.Host, poolConf.Port, poolConf.Name)
	for {
		clientConn, err := listener.Accept()
		if err != nil {
			LogError("Tunnel %s: Accept error: %v", tunnelName, err)
			continue
		}
		go handleConnection(tunnelName, clientConn, poolConf)
	}
}

func handleConnection(tunnelName string, clientConn net.Conn, poolConf PoolConfig) {
	defer clientConn.Close()
	clientAddr := clientConn.RemoteAddr().String()
	clientIP, clientPort, _ := net.SplitHostPort(clientAddr)
	LogDebug("New connection from %s:%s", clientIP, clientPort)
	poolAddr := fmt.Sprintf("%s:%d", poolConf.Host, poolConf.Port)
	poolConn, err := net.Dial("tcp", poolAddr)
	if err != nil {
		LogError("Failed to connect to pool %s: %v", poolAddr, err)
		return
	}
	defer poolConn.Close()
	LogInfo("Connected to pool %s (%s)", poolAddr, poolConf.Name)
	minerKey := fmt.Sprintf("%s:%s", clientIP, clientPort)
	minersMutex.Lock()
	miners[minerKey] = &MinerInfo{
		Name: "Unknown", IP: clientIP, Port: clientPort, PoolName: poolConf.Name,
		JobID: "", ConnectedAt: time.Now(), LastSeen: time.Now(),
	}
	minersMutex.Unlock()
	done := make(chan bool, 2)
	go func() {
		defer func() { done <- true }()
		proxyData(clientConn, poolConn, tunnelName, poolConf, minerKey, true)
	}()
	go func() {
		defer func() { done <- true }()
		proxyData(poolConn, clientConn, tunnelName, poolConf, minerKey, false)
	}()
	<-done
	LogWarning("Connection closed for %s:%s", clientIP, clientPort)
	minersMutex.Lock()
	if miner, exists := miners[minerKey]; exists {
		saveMinerToDB(miner)
		delete(miners, minerKey)
	}
	minersMutex.Unlock()
}

func proxyData(src, dst net.Conn, tunnelName string, poolConf PoolConfig, minerKey string, clientToPool bool) {
	scanner := bufio.NewScanner(src)
	for scanner.Scan() {
		line := scanner.Text()
		minersMutex.Lock()
		if miner, exists := miners[minerKey]; exists {
			if clientToPool {
				miner.BytesUpload += int64(len(line) + 1)
				miner.PacketsSent++
			} else {
				miner.BytesDownload += int64(len(line) + 1)
				miner.PacketsReceived++
			}
		}
		minersMutex.Unlock()
		if clientToPool {
			parseClientMessage(line, tunnelName, poolConf, minerKey)
		} else {
			parsePoolMessage(line, tunnelName, poolConf, minerKey)
		}
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
				parts := strings.Split(username, ".")
				miner.Wallet = parts[0]
				miner.Name = username
				LogInfo("Miner %s (%s:%s) authorized on tunnel %s -> %s",
					username, miner.IP, miner.Port, tunnelName, poolConf.Name)
			}
		}
	case "mining.submit":
		if params, ok := msg.Params.([]interface{}); ok && len(params) >= 3 {
			jobID := ""
			if jid, ok := params[1].(string); ok {
				jobID = jid
				miner.JobID = jobID
			}
			LogShare("Miner %s (%s:%s) job_id=%s * pending * pool %s port %d",
				miner.Name, miner.IP, miner.Port, jobID, poolConf.Name, poolConf.Port)
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
	if msg.Error != nil {
		LogError("Miner %s (%s:%s) * Error from pool %s: %v",
			miner.Name, miner.IP, miner.Port, poolConf.Name, msg.Error)
	}
	if msg.Method == "mining.notify" {
		if params, ok := msg.Params.([]interface{}); ok && len(params) > 0 {
			if jobID, ok := params[0].(string); ok {
				miner.JobID = jobID
				LogDebug("New job %s for miner %s (%s:%s) from pool %s",
					jobID, miner.Name, miner.IP, miner.Port, poolConf.Name)
			}
		}
	}
	if msg.ID != nil && msg.Result != nil {
		if result, ok := msg.Result.(bool); ok {
			submitTime := time.Since(miner.LastSeen).Seconds()
			if result {
				miner.SharesAccepted++
				poolMetricsMtx.Lock()
				if poolMetrics[poolConf.Name] == nil {
					poolMetrics[poolConf.Name] = &PoolMetrics{
						Name: poolConf.Name, PingSamples: make([]float64, 0), AcceptTimes: make([]float64, 0),
					}
				}
				poolMetrics[poolConf.Name].SharesAccepted++
				poolMetricsMtx.Unlock()
				recordAcceptTime(poolConf.Name, submitTime*1000)
				saveShareToDB(miner.Wallet, miner.Name, miner.IP, poolConf.Name, miner.JobID, true)
				LogShare("Share ACCEPTED for miner %s (%s:%s) from pool %s (%.2fms)",
					miner.Name, miner.IP, miner.Port, poolConf.Name, submitTime*1000)
			} else {
				miner.SharesRejected++
				poolMetricsMtx.Lock()
				if poolMetrics[poolConf.Name] == nil {
					poolMetrics[poolConf.Name] = &PoolMetrics{
						Name: poolConf.Name, PingSamples: make([]float64, 0), AcceptTimes: make([]float64, 0),
					}
				}
				poolMetrics[poolConf.Name].SharesRejected++
				poolMetricsMtx.Unlock()
				saveShareToDB(miner.Wallet, miner.Name, miner.IP, poolConf.Name, miner.JobID, false)
				LogError("Share REJECTED for miner %s (%s:%s) from pool %s",
					miner.Name, miner.IP, miner.Port, poolConf.Name)
			}
		}
	}
	miner.LastSeen = time.Now()
	minersMutex.Unlock()
}

func handleLogStream() {
	for {
		msg := <-logBroadcast
		logClientsMtx.RLock()
		for client := range logClients {
			err := client.WriteJSON(msg)
			if err != nil {
				client.Close()
				logClientsMtx.RUnlock()
				logClientsMtx.Lock()
				delete(logClients, client)
				logClientsMtx.Unlock()
				logClientsMtx.RLock()
			}
		}
		logClientsMtx.RUnlock()
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	logClientsMtx.Lock()
	logClients[conn] = true
	logClientsMtx.Unlock()
	conn.WriteJSON(LogMessage{
		Timestamp: getCurrentTime(),
		Level:     "INFO",
		Message:   "Connected to log stream",
		Color:     "green",
	})
	defer func() {
		logClientsMtx.Lock()
		delete(logClients, conn)
		logClientsMtx.Unlock()
		conn.Close()
	}()
	for {
		if _, _, err := conn.NextReader(); err != nil {
			break
		}
	}
}

func LogAndBroadcast(level, color, format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	timestamp := getCurrentTime()
	fmt.Printf("%s[%s]%s %s %s\n", color, level, ColorReset, timestamp, message)
	logMsg := LogMessage{
		Timestamp: timestamp,
		Level:     level,
		Message:   message,
		Color:     getColorName(color),
	}
	select {
	case logBroadcast <- logMsg:
	default:
	}
}

func LogInfo(format string, args ...interface{}) {
	LogAndBroadcast("INFO", ColorGreen, format, args...)
}

func LogError(format string, args ...interface{}) {
	LogAndBroadcast("ERROR", ColorRed, format, args...)
}

func LogWarning(format string, args ...interface{}) {
	LogAndBroadcast("WARN", ColorYellow, format, args...)
}

func LogShare(format string, args ...interface{}) {
	LogAndBroadcast("SHARE", ColorPurple, format, args...)
}

func LogDebug(format string, args ...interface{}) {
	LogAndBroadcast("DEBUG", ColorCyan, format, args...)
}

func getColorName(colorCode string) string {
	switch colorCode {
	case ColorRed:
		return "red"
	case ColorGreen:
		return "green"
	case ColorYellow:
		return "yellow"
	case ColorBlue:
		return "blue"
	case ColorPurple:
		return "purple"
	case ColorCyan:
		return "cyan"
	default:
		return "white"
	}
}

func startAPIServer() {
	http.HandleFunc("/api/metrics", handleAPIMetrics)
	http.HandleFunc("/api/i/", handleMinerInfo)
	http.HandleFunc("/metrics", handlePrometheusMetrics)
	http.HandleFunc("/api/logs/stream", handleWebSocket)
	go handleLogStream()
	addr := fmt.Sprintf(":%d", config.APIPort)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func handleAPIMetrics(w http.ResponseWriter, r *http.Request) {
	systemMtx.RLock()
	poolMetricsMtx.RLock()
	minersMutex.RLock()
	poolsData := make(map[string]interface{})
	for name, pm := range poolMetrics {
		poolsData[name] = map[string]interface{}{
			"current_ping_ms":    pm.CurrentPing,
			"average_ping_ms":    pm.AveragePing,
			"avg_accept_time_ms": pm.AvgAcceptTime,
			"shares_accepted":    pm.SharesAccepted,
			"shares_rejected":    pm.SharesRejected,
			"last_ping_time":     pm.LastPingTime.Format(time.RFC3339),
		}
	}
	response := map[string]interface{}{
		"system": map[string]interface{}{
			"cpu_model":       systemMetrics.CPUModel,
			"cpu_cores":       systemMetrics.CPUCores,
			"ram_total_bytes": systemMetrics.RAMTotal,
			"ram_used_bytes":  systemMetrics.RAMUsed,
			"disk_total_bytes": systemMetrics.DiskTotal,
			"disk_used_bytes": systemMetrics.DiskUsed,
			"os":              systemMetrics.OS,
			"public_ip":       systemMetrics.PublicIP,
			"uptime_seconds":  systemMetrics.Uptime.Seconds(),
		},
		"network": map[string]interface{}{
			"total_download_bytes": systemMetrics.TotalDownload,
			"total_upload_bytes":   systemMetrics.TotalUpload,
			"packets_sent":         systemMetrics.PacketsSent,
			"packets_received":     systemMetrics.PacketsReceived,
		},
		"miners": map[string]interface{}{
			"active_count": systemMetrics.ActiveMiners,
		},
		"pools": poolsData,
	}
	minersMutex.RUnlock()
	poolMetricsMtx.RUnlock()
	systemMtx.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleMinerInfo(w http.ResponseWriter, r *http.Request) {
	wallet := strings.TrimPrefix(r.URL.Path, "/api/i/")
	if wallet == "" {
		http.Error(w, "Wallet address required", http.StatusBadRequest)
		return
	}
	query := `SELECT wallet, miner_name, ip, pool_name, shares_accepted, shares_rejected,
		bytes_download, bytes_upload, packets_sent, packets_received, connected_at, last_seen
		FROM miners WHERE wallet LIKE ?`
	rows, err := db.Query(query, wallet+"%")
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var minersList []map[string]interface{}
	for rows.Next() {
		var (
			dbWallet, minerName, ip, poolName                        string
			sharesAccepted, sharesRejected                           int
			bytesDownload, bytesUpload, packetsSent, packetsReceived int64
			connectedAt, lastSeen                                    time.Time
		)
		err := rows.Scan(&dbWallet, &minerName, &ip, &poolName, &sharesAccepted, &sharesRejected,
			&bytesDownload, &bytesUpload, &packetsSent, &packetsReceived, &connectedAt, &lastSeen)
		if err != nil {
			continue
		}
		uptime := lastSeen.Sub(connectedAt).Seconds()
		minerData := map[string]interface{}{
			"wallet": dbWallet, "miner_name": minerName, "ip": ip, "pool_name": poolName,
			"shares_accepted": sharesAccepted, "shares_rejected": sharesRejected,
			"bytes_download": bytesDownload, "bytes_upload": bytesUpload,
			"packets_sent": packetsSent, "packets_received": packetsReceived,
			"uptime_seconds": uptime, "connected_at": connectedAt.Format(time.RFC3339),
			"last_seen": lastSeen.Format(time.RFC3339),
		}
		minersList = append(minersList, minerData)
	}
	minersMutex.RLock()
	var activeMinerData map[string]interface{}
	for _, miner := range miners {
		if miner.Wallet != "" && strings.HasPrefix(miner.Wallet, wallet) {
			uptime := time.Since(miner.ConnectedAt).Seconds()
			activeMinerData = map[string]interface{}{
				"wallet": miner.Wallet, "miner_name": miner.Name, "ip": miner.IP, "pool_name": miner.PoolName,
				"shares_accepted": miner.SharesAccepted, "shares_rejected": miner.SharesRejected,
				"bytes_download": miner.BytesDownload, "bytes_upload": miner.BytesUpload,
				"packets_sent": miner.PacketsSent, "packets_received": miner.PacketsReceived,
				"uptime_seconds": uptime, "connected_at": miner.ConnectedAt.Format(time.RFC3339),
				"last_seen": miner.LastSeen.Format(time.RFC3339), "status": "online",
			}
			break
		}
	}
	minersMutex.RUnlock()
	response := map[string]interface{}{
		"wallet": wallet, "active_miner": activeMinerData,
		"historical_data": minersList, "total_miners": len(minersList),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handlePrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	systemMtx.RLock()
	poolMetricsMtx.RLock()
	minersMutex.RLock()
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "# HELP mining_tunnel_uptime_seconds Uptime in seconds\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_uptime_seconds gauge\n")
	fmt.Fprintf(w, "mining_tunnel_uptime_seconds %.2f\n\n", systemMetrics.Uptime.Seconds())
	fmt.Fprintf(w, "# HELP mining_tunnel_active_miners Number of active miners\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_active_miners gauge\n")
	fmt.Fprintf(w, "mining_tunnel_active_miners %d\n\n", systemMetrics.ActiveMiners)
	fmt.Fprintf(w, "# HELP mining_tunnel_bytes_total Total bytes transferred\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_bytes_total counter\n")
	fmt.Fprintf(w, "mining_tunnel_bytes_total{direction=\"download\"} %d\n", systemMetrics.TotalDownload)
	fmt.Fprintf(w, "mining_tunnel_bytes_total{direction=\"upload\"} %d\n\n", systemMetrics.TotalUpload)
	fmt.Fprintf(w, "# HELP mining_tunnel_packets_total Total packets transferred\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_packets_total counter\n")
	fmt.Fprintf(w, "mining_tunnel_packets_total{direction=\"sent\"} %d\n", systemMetrics.PacketsSent)
	fmt.Fprintf(w, "mining_tunnel_packets_total{direction=\"received\"} %d\n\n", systemMetrics.PacketsReceived)
	fmt.Fprintf(w, "# HELP mining_tunnel_cpu_cores Number of CPU cores\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_cpu_cores gauge\n")
	fmt.Fprintf(w, "mining_tunnel_cpu_cores %d\n\n", systemMetrics.CPUCores)
	fmt.Fprintf(w, "# HELP mining_tunnel_ram_bytes RAM usage in bytes\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_ram_bytes gauge\n")
	fmt.Fprintf(w, "mining_tunnel_ram_bytes{type=\"total\"} %d\n", systemMetrics.RAMTotal)
	fmt.Fprintf(w, "mining_tunnel_ram_bytes{type=\"used\"} %d\n\n", systemMetrics.RAMUsed)
	fmt.Fprintf(w, "# HELP mining_tunnel_disk_bytes Disk usage in bytes\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_disk_bytes gauge\n")
	fmt.Fprintf(w, "mining_tunnel_disk_bytes{type=\"total\"} %d\n", systemMetrics.DiskTotal)
	fmt.Fprintf(w, "mining_tunnel_disk_bytes{type=\"used\"} %d\n\n", systemMetrics.DiskUsed)
	for poolName, pm := range poolMetrics {
		fmt.Fprintf(w, "# HELP mining_tunnel_pool_ping_ms Pool ping in milliseconds\n")
		fmt.Fprintf(w, "# TYPE mining_tunnel_pool_ping_ms gauge\n")
		fmt.Fprintf(w, "mining_tunnel_pool_ping_ms{pool=\"%s\",type=\"current\"} %.2f\n", poolName, pm.CurrentPing)
		fmt.Fprintf(w, "mining_tunnel_pool_ping_ms{pool=\"%s\",type=\"average\"} %.2f\n\n", poolName, pm.AveragePing)
		fmt.Fprintf(w, "# HELP mining_tunnel_pool_accept_time_ms Average accept time in milliseconds\n")
		fmt.Fprintf(w, "# TYPE mining_tunnel_pool_accept_time_ms gauge\n")
		fmt.Fprintf(w, "mining_tunnel_pool_accept_time_ms{pool=\"%s\"} %.2f\n\n", poolName, pm.AvgAcceptTime)
		fmt.Fprintf(w, "# HELP mining_tunnel_pool_shares_total Total shares by pool\n")
		fmt.Fprintf(w, "# TYPE mining_tunnel_pool_shares_total counter\n")
		fmt.Fprintf(w, "mining_tunnel_pool_shares_total{pool=\"%s\",status=\"accepted\"} %d\n", poolName, pm.SharesAccepted)
		fmt.Fprintf(w, "mining_tunnel_pool_shares_total{pool=\"%s\",status=\"rejected\"} %d\n\n", poolName, pm.SharesRejected)
	}
	minersMutex.RUnlock()
	poolMetricsMtx.RUnlock()
	systemMtx.RUnlock()
}

func getCurrentTime() string {
	return time.Now().Format("2025-06-03 15:04:05")
}

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}
