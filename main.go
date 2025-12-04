package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"crypto/tls"
	"fmt"
	"log"
	"flag" 
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	VERSION     = "3.3"
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
	SharesAccepted  int64
	SharesRejected  int64
	LastSeen        time.Time
	ConnectedAt     time.Time
	BytesDownload   int64
	BytesUpload     int64
	PacketsSent     int64
	PacketsReceived int64
	LastShareTime   time.Time
	ShareTimes      []time.Time
	CurrentHashrate float64
	AverageHashrate float64
	Difficulty      float64
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
	CPUUsage        float64
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
	DataDBSize      int64
	SystemDBSize    int64
}

type LogMessage struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Color     string `json:"color"`
}

var (
	tlsConfig *tls.Config
	config         Config
	dataDB         *sql.DB
	systemDB       *sql.DB
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
	
	totalDownloadAtomic   int64
	totalUploadAtomic     int64
	packetsSentAtomic     int64
	packetsReceivedAtomic int64
	
	shareBatchChan   = make(chan ShareRecord, 10000)
	trafficBatchChan = make(chan TrafficRecord, 10000)
	flagNoData  = flag.Bool("nodata", false, "Disable database logging")
	flagNoAPI   = flag.Bool("noapi", false, "Disable API server")
	flagNoDebug = flag.Bool("nodebug", false, "Minimal output mode (single line status)")
	flagTLS     = flag.Bool("tls", false, "Enable TLS support for miner connections")
	flagHelp    = flag.Bool("help", false, "Show help message")
	flagVersion = flag.Bool("version", false, "Show version")
	flagTLSCert = flag.String("tlscert", "cert.pem", "TLS certificate file")
	flagTLSKey  = flag.String("tlskey", "key.pem", "TLS key file")
)

type ShareRecord struct {
	Wallet      string
	MinerName   string
	IP          string
	PoolName    string
	JobID       string
	Accepted    bool
	Difficulty  float64
	SubmittedAt time.Time
}

type TrafficRecord struct {
	Timestamp       time.Time
	BytesDownload   int64
	BytesUpload     int64
	PacketsSent     int64
	PacketsReceived int64
}

func showHelp() {
	fmt.Printf("Tunnel v%s - Mining Pool Proxy\n\n", VERSION)
	fmt.Println("Usage: tunnel [options]")
	fmt.Println("\nOptions:")
	fmt.Println("  --nodata       Disable database logging")
	fmt.Println("  --noapi        Disable API server")
	fmt.Println("  --nodebug      Minimal output mode (single line status)")
	fmt.Println("  --tls          Enable TLS support for miner connections")
	fmt.Println("  --tlscert      TLS certificate file (default: cert.pem)")
	fmt.Println("  --tlskey       TLS key file (default: key.pem)")
	fmt.Println("  --help         Show this help message")
	fmt.Println("  --version      Show version")
	fmt.Println("\nExamples:")
	fmt.Println("  tunnel                    # Run with default settings")
	fmt.Println("  tunnel --nodata           # Run without database")
	fmt.Println("  tunnel --nodebug          # Run with minimal output")
	fmt.Println("  tunnel --tls              # Run with TLS support")
	fmt.Println("  tunnel --nodata --noapi   # Run without database and API")
}

func Status() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		minersMutex.RLock()
		activeMiners := len(miners)
		minersMutex.RUnlock()
		
		uptime := time.Since(startTime)
		uptimeStr := formatDuration(uptime)
		fmt.Printf("\r\033[K")
		fmt.Printf("Tunnel v%s | Uptime: %s | Miners: %d", 
			VERSION, uptimeStr, activeMiners)
	}
}

func formatDuration(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60
	
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	} else if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
func main() {
	flag.Parse()
	fmt.Printf("Tunnel v%s by mytai\n%s\n", VERSION, strings.Repeat("-", 60))
	if *flagVersion {
		fmt.Printf("Tunnel v%s\n", VERSION)
		return
	}
	if *flagHelp {
		showHelp()
		return
	}
	startTime = time.Now()
	loadConfig()
	if !*flagNoData {
		initDatabases()
		go batchShareProcessor()
		go batchTrafficProcessor()
		go periodicTrafficSnapshot()
		go cleanupOldData()
	}
	initSystemMetrics()
	if *flagTLS {
		cert, err := tls.LoadX509KeyPair(*flagTLSCert, *flagTLSKey)
		if err != nil {
			log.Fatalf("Failed to load TLS certificate: %v", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		if !*flagNoDebug {
			LogInfo("TLS enabled with cert: %s, key: %s", *flagTLSCert, *flagTLSKey)
		}
	}
	var wg sync.WaitGroup
	for tunnelName, tunnelConf := range config.Tunnels {
		wg.Add(1)
		go func(name string, conf TunnelConfig) {
			defer wg.Done()
			startTunnel(name, conf)
		}(tunnelName, tunnelConf)
	}
	if !*flagNoAPI {
		go startAPIServer()
	}
	
	go monitorPoolPings()
	go updateSystemMetrics()
	if !*flagNoDebug {
		LogInfo("Loaded %d pools", len(config.Pools))
		LogInfo("Tunnel Started")
		LogInfo("Active tunnels: %d", len(config.Tunnels))
		if !*flagNoAPI {
			LogInfo("API server running on port %d", config.APIPort)
		}
		if *flagTLS {
			LogInfo("TLS support enabled")
		}
		if *flagNoData {
			LogInfo("Database logging disabled")
		}
		fmt.Printf("%s\n", strings.Repeat("-", 60))
	} else {
		go Status()
	}
	
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
		if !*flagNoDebug {
			LogWarning("Created default config.yml")
		}
	} else {
		data, err := os.ReadFile(configFile)
		if err != nil {
			log.Fatalf("Error reading config: %v", err)
		}
		err = yaml.Unmarshal(data, &config)
		if err != nil {
			log.Fatalf("Error parsing config: %v", err)
		}
		if !*flagNoDebug {
			LogInfo("Loaded config.yml")
		}
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

func initDatabases() {
	var err error
	
	dataDB, err = sql.Open("sqlite", "./data.db?cache=shared&mode=rwc")
	if err != nil {
		log.Fatalf("Failed to connect to data.db: %v", err)
	}
	dataDB.SetMaxOpenConns(25)
	dataDB.SetMaxIdleConns(5)
	dataDB.SetConnMaxLifetime(5 * time.Minute)
	
	if err = dataDB.Ping(); err != nil {
		log.Fatalf("Failed to ping data.db: %v", err)
	}
	
	systemDB, err = sql.Open("sqlite", "./system.db?cache=shared&mode=rwc&_journal_mode=WAL")
	if err != nil {
		log.Fatalf("Failed to connect to system.db: %v", err)
	}
	systemDB.SetMaxOpenConns(50)
	systemDB.SetMaxIdleConns(10)
	systemDB.SetConnMaxLifetime(5 * time.Minute)
	
	if err = systemDB.Ping(); err != nil {
		log.Fatalf("Failed to ping system.db: %v", err)
	}
	
	createTables()
	if !*flagNoDebug {
		LogInfo("Database connected (data.db + system.db - Pure Go)")
	}
}

func createTables() {
	dataQueries := []string{
		`CREATE TABLE IF NOT EXISTS miners (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			wallet TEXT NOT NULL,
			miner_name TEXT,
			ip TEXT,
			pool_name TEXT,
			shares_accepted INTEGER DEFAULT 0,
			shares_rejected INTEGER DEFAULT 0,
			bytes_download INTEGER DEFAULT 0,
			bytes_upload INTEGER DEFAULT 0,
			packets_sent INTEGER DEFAULT 0,
			packets_received INTEGER DEFAULT 0,
			current_hashrate REAL DEFAULT 0,
			average_hashrate REAL DEFAULT 0,
			connected_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(wallet, ip, miner_name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_wallet ON miners(wallet)`,
		`CREATE INDEX IF NOT EXISTS idx_ip ON miners(ip)`,
		`CREATE INDEX IF NOT EXISTS idx_last_seen ON miners(last_seen)`,
	}
	
	for _, query := range dataQueries {
		if _, err := dataDB.Exec(query); err != nil {
			log.Fatalf("Failed to create table in data.db: %v", err)
		}
	}
	
	systemQueries := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA cache_size=10000`,
		`PRAGMA temp_store=MEMORY`,
		`CREATE TABLE IF NOT EXISTS shares (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			wallet TEXT NOT NULL,
			miner_name TEXT,
			ip TEXT,
			pool_name TEXT,
			job_id TEXT,
			accepted INTEGER,
			difficulty REAL DEFAULT 0,
			submitted_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_wallet ON shares(wallet)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_submitted ON shares(submitted_at)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_pool ON shares(pool_name)`,
		`CREATE TABLE IF NOT EXISTS network_traffic (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			bytes_download INTEGER DEFAULT 0,
			bytes_upload INTEGER DEFAULT 0,
			packets_sent INTEGER DEFAULT 0,
			packets_received INTEGER DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_timestamp ON network_traffic(timestamp)`,
	}
	
	for _, query := range systemQueries {
		if _, err := systemDB.Exec(query); err != nil {
			log.Fatalf("Failed to create table in system.db: %v", err)
		}
	}
}

func batchShareProcessor() {
	if *flagNoData {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	
	batch := make([]ShareRecord, 0, 1000)
	
	for {
		select {
		case share := <-shareBatchChan:
			batch = append(batch, share)
			if len(batch) >= 1000 {
				insertShareBatch(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				insertShareBatch(batch)
				batch = batch[:0]
			}
		}
	}
}

func insertShareBatch(batch []ShareRecord) {
	if len(batch) == 0 {
		return
	}
	
	tx, err := systemDB.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v", err)
		return
	}
	
	stmt, err := tx.Prepare(`INSERT INTO shares 
		(wallet, miner_name, ip, pool_name, job_id, accepted, difficulty, submitted_at) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to prepare statement: %v", err)
		return
	}
	defer stmt.Close()
	
	for _, share := range batch {
		acceptedInt := 0
		if share.Accepted {
			acceptedInt = 1
		}
		_, err := stmt.Exec(share.Wallet, share.MinerName, share.IP, share.PoolName,
			share.JobID, acceptedInt, share.Difficulty, share.SubmittedAt)
		if err != nil {
			log.Printf("Failed to insert share: %v", err)
		}
	}
	
	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v", err)
	}
}

func batchTrafficProcessor() {
	if *flagNoData {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	
	batch := make([]TrafficRecord, 0, 100)
	
	for {
		select {
		case traffic := <-trafficBatchChan:
			batch = append(batch, traffic)
			if len(batch) >= 100 {
				insertTrafficBatch(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				insertTrafficBatch(batch)
				batch = batch[:0]
			}
		}
	}
}

func insertTrafficBatch(batch []TrafficRecord) {
	if len(batch) == 0 {
		return
	}
	
	tx, err := systemDB.Begin()
	if err != nil {
		return
	}
	
	stmt, err := tx.Prepare(`INSERT INTO network_traffic 
		(timestamp, bytes_download, bytes_upload, packets_sent, packets_received) 
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return
	}
	defer stmt.Close()
	
	for _, traffic := range batch {
		stmt.Exec(traffic.Timestamp, traffic.BytesDownload, traffic.BytesUpload,
			traffic.PacketsSent, traffic.PacketsReceived)
	}
	
	tx.Commit()
}

func periodicTrafficSnapshot() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		download := atomic.LoadInt64(&totalDownloadAtomic)
		upload := atomic.LoadInt64(&totalUploadAtomic)
		sent := atomic.LoadInt64(&packetsSentAtomic)
		received := atomic.LoadInt64(&packetsReceivedAtomic)
		
		select {
		case trafficBatchChan <- TrafficRecord{
			Timestamp:       time.Now(),
			BytesDownload:   download,
			BytesUpload:     upload,
			PacketsSent:     sent,
			PacketsReceived: received,
		}:
		default:
		}
	}
}

func cleanupOldData() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	
	for range ticker.C {
		_, err := systemDB.Exec(`DELETE FROM shares WHERE submitted_at < datetime('now', '-365 days')`)
		if err != nil {
			log.Printf("Failed to cleanup old shares: %v", err)
		}
		
		_, err = systemDB.Exec(`DELETE FROM network_traffic WHERE timestamp < datetime('now', '-180 days')`)
		if err != nil {
			log.Printf("Failed to cleanup old traffic: %v", err)
		}
		
		systemDB.Exec("VACUUM")
		dataDB.Exec("VACUUM")
		
		if !*flagNoDebug {
			LogInfo("Database cleanup completed")
		}
	}
}

func getDBSize(dbPath string) int64 {
	info, err := os.Stat(dbPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

func saveMinerToDB(miner *MinerInfo) {
	if *flagNoData {
		return
	}
	
	checkQuery := `SELECT id, shares_accepted, shares_rejected, bytes_download, bytes_upload, 
		packets_sent, packets_received FROM miners 
		WHERE wallet = ? AND ip = ? AND miner_name = ?`
	var existingID int
	var existingAccepted, existingRejected int64
	var existingDownload, existingUpload, existingSent, existingReceived int64
	
	err := dataDB.QueryRow(checkQuery, miner.Wallet, miner.IP, miner.Name).Scan(
		&existingID, &existingAccepted, &existingRejected,
		&existingDownload, &existingUpload, &existingSent, &existingReceived)
	
	if err == sql.ErrNoRows {
		insertQuery := `INSERT INTO miners (wallet, miner_name, ip, pool_name, shares_accepted, 
			shares_rejected, bytes_download, bytes_upload, packets_sent, packets_received,
			current_hashrate, average_hashrate, connected_at, last_seen) 
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		_, err = dataDB.Exec(insertQuery, miner.Wallet, miner.Name, miner.IP, miner.PoolName,
			miner.SharesAccepted, miner.SharesRejected, miner.BytesDownload, miner.BytesUpload,
			miner.PacketsSent, miner.PacketsReceived, miner.CurrentHashrate, miner.AverageHashrate,
			miner.ConnectedAt, miner.LastSeen)
	} else if err == nil {
		updateQuery := `UPDATE miners SET shares_accepted = ?, shares_rejected = ?,
			bytes_download = ?, bytes_upload = ?, packets_sent = ?, packets_received = ?,
			current_hashrate = ?, average_hashrate = ?, last_seen = ?, pool_name = ? WHERE id = ?`
		_, err = dataDB.Exec(updateQuery,
			existingAccepted+miner.SharesAccepted, existingRejected+miner.SharesRejected,
			existingDownload+miner.BytesDownload, existingUpload+miner.BytesUpload,
			existingSent+miner.PacketsSent, existingReceived+miner.PacketsReceived,
			miner.CurrentHashrate, miner.AverageHashrate,
			miner.LastSeen, miner.PoolName, existingID)
	}
	
	if err != nil && !*flagNoDebug {
		log.Printf("Failed to save miner to DB: %v", err)
	}
}
func formatHashrate(hashrate float64) string {
	if hashrate == 0 {
		return "0 H/s"
	}
	
	units := []string{"H/s", "KH/s", "MH/s", "GH/s", "TH/s", "PH/s"}
	unitIndex := 0
	value := hashrate
	
	// Chia cho 1000 thay vì 1024
	for value >= 1000 && unitIndex < len(units)-1 {
		value /= 1000
		unitIndex++
	}
	
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[unitIndex])
	} else if value >= 10 {
		return fmt.Sprintf("%.1f %s", value, units[unitIndex])
	} else {
		return fmt.Sprintf("%.2f %s", value, units[unitIndex])
	}
}
func calculateHashrate(miner *MinerInfo) {
	now := time.Now()
	
	// Chỉ giữ shares trong 10 phút gần nhất
	cutoff := now.Add(-10 * time.Minute)
	validShares := make([]time.Time, 0)
	for _, t := range miner.ShareTimes {
		if t.After(cutoff) {
			validShares = append(validShares, t)
		}
	}
	miner.ShareTimes = validShares
	
	if len(miner.ShareTimes) < 2 {
		miner.CurrentHashrate = 0
		return
	}
	
	difficulty := miner.Difficulty
	if difficulty == 0 {
		difficulty = 1.0
	}
	
	// Tính hashrate đơn giản: (số shares * difficulty) / thời gian
	totalTime := miner.ShareTimes[len(miner.ShareTimes)-1].Sub(miner.ShareTimes[0]).Seconds()
	if totalTime > 0 {
		// Hashrate = (shares * difficulty * 2^32) / time
		// Nhưng để đơn giản và chính xác hơn, ta dùng công thức đơn giản:
		sharesPerSecond := float64(len(miner.ShareTimes)) / totalTime
		// Difficulty đã tính sẵn, không cần nhân 2^32
		miner.CurrentHashrate = sharesPerSecond * difficulty
	}
	
	// Tính average hashrate (EMA - Exponential Moving Average)
	if miner.AverageHashrate == 0 {
		miner.AverageHashrate = miner.CurrentHashrate
	} else {
		// Smooth với 90% giá trị cũ + 10% giá trị mới
		miner.AverageHashrate = (miner.AverageHashrate * 0.9) + (miner.CurrentHashrate * 0.1)
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
		
		cpuPercent, _ := cpu.Percent(time.Second, false)
		if len(cpuPercent) > 0 {
			systemMetrics.CPUUsage = cpuPercent[0]
		}
		
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
		minersMutex.RUnlock()
		
		systemMetrics.TotalDownload = atomic.LoadInt64(&totalDownloadAtomic)
		systemMetrics.TotalUpload = atomic.LoadInt64(&totalUploadAtomic)
		systemMetrics.PacketsSent = atomic.LoadInt64(&packetsSentAtomic)
		systemMetrics.PacketsReceived = atomic.LoadInt64(&packetsReceivedAtomic)
		
		systemMetrics.DataDBSize = getDBSize("./data.db")
		systemMetrics.SystemDBSize = getDBSize("./system.db")
		
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
	
	var listener net.Listener
	var err error
	
	// Create listener with or without TLS
	if *flagTLS && tlsConfig != nil {
		listener, err = tls.Listen("tcp", addr, tlsConfig)
		if err != nil {
			LogError("Tunnel %s: Failed to listen on %s (TLS): %v", tunnelName, addr, err)
			return
		}
		if !*flagNoDebug {
			LogDebug("Tunnel %s listening on %s (TLS) -> %s:%d (%s)", tunnelName, addr, poolConf.Host, poolConf.Port, poolConf.Name)
		}
	} else {
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			LogError("Tunnel %s: Failed to listen on %s: %v", tunnelName, addr, err)
			return
		}
		if !*flagNoDebug {
			LogDebug("Tunnel %s listening on %s -> %s:%d (%s)", tunnelName, addr, poolConf.Host, poolConf.Port, poolConf.Name)
		}
	}
	defer listener.Close()
	
	for {
		clientConn, err := listener.Accept()
		if err != nil {
			if !*flagNoDebug {
				LogError("Tunnel %s: Accept error: %v", tunnelName, err)
			}
			continue
		}
		go handleConnection(tunnelName, clientConn, poolConf)
	}
}

func handleConnection(tunnelName string, clientConn net.Conn, poolConf PoolConfig) {
	defer clientConn.Close()
	
	clientAddr := clientConn.RemoteAddr().String()
	clientIP, clientPort, _ := net.SplitHostPort(clientAddr)
	if !*flagNoDebug {
		LogDebug("New connection from %s:%s", clientIP, clientPort)
	}
	
	poolAddr := fmt.Sprintf("%s:%d", poolConf.Host, poolConf.Port)
	poolConn, err := net.Dial("tcp", poolAddr)
	if err != nil {
		if !*flagNoDebug {
			LogError("Failed to connect to pool %s: %v", poolAddr, err)
		}
		return
	}
	defer poolConn.Close()
	
	if !*flagNoDebug {
		LogInfo("Connected to pool %s (%s)", poolAddr, poolConf.Name)
	}
	
	minerKey := fmt.Sprintf("%s:%s", clientIP, clientPort)
	minersMutex.Lock()
	miners[minerKey] = &MinerInfo{
		Name: "Unknown", IP: clientIP, Port: clientPort, PoolName: poolConf.Name,
		JobID: "", ConnectedAt: time.Now(), LastSeen: time.Now(),
		ShareTimes: make([]time.Time, 0),
		Difficulty: 1.0,
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
	
	if !*flagNoDebug {
		LogWarning("Connection closed for %s:%s", clientIP, clientPort)
	}
	
	minersMutex.Lock()
	if miner, exists := miners[minerKey]; exists {
		saveMinerToDB(miner)
		delete(miners, minerKey)
	}
	minersMutex.Unlock()
}

func proxyData(src, dst net.Conn, tunnelName string, poolConf PoolConfig, minerKey string, clientToPool bool) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	
	for scanner.Scan() {
		line := scanner.Text()
		lineLen := int64(len(line) + 1)
		
		if clientToPool {
			atomic.AddInt64(&totalUploadAtomic, lineLen)
			atomic.AddInt64(&packetsSentAtomic, 1)
		} else {
			atomic.AddInt64(&totalDownloadAtomic, lineLen)
			atomic.AddInt64(&packetsReceivedAtomic, 1)
		}
		
		minersMutex.Lock()
		if miner, exists := miners[minerKey]; exists {
			if clientToPool {
				miner.BytesUpload += lineLen
				miner.PacketsSent++
			} else {
				miner.BytesDownload += lineLen
				miner.PacketsReceived++
			}
			miner.LastSeen = time.Now()
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
			
			miner.LastShareTime = time.Now()
			miner.ShareTimes = append(miner.ShareTimes, time.Now())
			
			LogShare("Share submitted: %s (%s:%s) job=%s pool=%s",
				miner.Name, miner.IP, miner.Port, jobID, poolConf.Name)
		}
	case "mining.extranonce.subscribe":
		// Handle extranonce subscription
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
	
	if msg.Method == "mining.set_difficulty" {
		if params, ok := msg.Params.([]interface{}); ok && len(params) > 0 {
			if diff, ok := params[0].(float64); ok {
				miner.Difficulty = diff
				LogDebug("Difficulty set to %.2f for miner %s", diff, miner.Name)
			}
		}
	}
	
	if msg.ID != nil && msg.Result != nil {
		if result, ok := msg.Result.(bool); ok {
			submitTime := time.Since(miner.LastShareTime).Seconds()
			
			if result {
				miner.SharesAccepted++
				
				calculateHashrate(miner)
				
				poolMetricsMtx.Lock()
				if poolMetrics[poolConf.Name] == nil {
					poolMetrics[poolConf.Name] = &PoolMetrics{
						Name: poolConf.Name, PingSamples: make([]float64, 0), AcceptTimes: make([]float64, 0),
					}
				}
				poolMetrics[poolConf.Name].SharesAccepted++
				poolMetricsMtx.Unlock()
				
				recordAcceptTime(poolConf.Name, submitTime*1000)
				
				select {
				case shareBatchChan <- ShareRecord{
					Wallet:      miner.Wallet,
					MinerName:   miner.Name,
					IP:          miner.IP,
					PoolName:    poolConf.Name,
					JobID:       miner.JobID,
					Accepted:    true,
					Difficulty:  miner.Difficulty,
					SubmittedAt: time.Now(),
				}:
				default:
					LogWarning("Share batch channel full, dropping share record")
				}
				
				LogShare("✓ ACCEPTED: %s (%s:%s) pool=%s (%.0fms) [%s avg=%s]",
					miner.Name, miner.IP, miner.Port, poolConf.Name, submitTime*1000, 
					formatHashrate(miner.CurrentHashrate), formatHashrate(miner.AverageHashrate))
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
				
				select {
				case shareBatchChan <- ShareRecord{
					Wallet:      miner.Wallet,
					MinerName:   miner.Name,
					IP:          miner.IP,
					PoolName:    poolConf.Name,
					JobID:       miner.JobID,
					Accepted:    false,
					Difficulty:  miner.Difficulty,
					SubmittedAt: time.Now(),
				}:
				default:
				}
				
				LogError("✗ REJECTED: %s (%s:%s) pool=%s",
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
	if !*flagNoDebug {
		LogAndBroadcast("INFO", ColorGreen, format, args...)
	}
}

func LogError(format string, args ...interface{}) {
	if !*flagNoDebug {
		LogAndBroadcast("ERROR", ColorRed, format, args...)
	}
}

func LogWarning(format string, args ...interface{}) {
	if !*flagNoDebug {
		LogAndBroadcast("WARN", ColorYellow, format, args...)
	}
}

func LogShare(format string, args ...interface{}) {
	if !*flagNoDebug {
		LogAndBroadcast("SHARE", ColorPurple, format, args...)
	}
}

func LogDebug(format string, args ...interface{}) {
	if !*flagNoDebug {
		LogAndBroadcast("DEBUG", ColorCyan, format, args...)
	}
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
	http.HandleFunc("/api/network/stats", handleNetworkStats)
	http.HandleFunc("/api/shares/stats", handleSharesStats)
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
	
	minersData := make([]map[string]interface{}, 0)
	for _, miner := range miners {
		minersData = append(minersData, map[string]interface{}{
			"wallet":           miner.Wallet,
			"name":             miner.Name,
			"ip":               miner.IP,
			"pool":             miner.PoolName,
			"shares_accepted":  miner.SharesAccepted,
			"shares_rejected":  miner.SharesRejected,
			"current_hashrate": formatHashrate(miner.CurrentHashrate),
			"average_hashrate": formatHashrate(miner.AverageHashrate),
			"difficulty":       miner.Difficulty,
			"uptime_seconds":   time.Since(miner.ConnectedAt).Seconds(),
		})
	}
	
	response := map[string]interface{}{
		"system": map[string]interface{}{
			"cpu_model":          systemMetrics.CPUModel,
			"cpu_cores":          systemMetrics.CPUCores,
			"cpu_usage_percent":  fmt.Sprintf("%.2f%%", systemMetrics.CPUUsage),
			"ram_total_bytes":    systemMetrics.RAMTotal,
			"ram_used_bytes":     systemMetrics.RAMUsed,
			"ram_usage_percent":  fmt.Sprintf("%.2f%%", float64(systemMetrics.RAMUsed)/float64(systemMetrics.RAMTotal)*100),
			"disk_total_bytes":   systemMetrics.DiskTotal,
			"disk_used_bytes":    systemMetrics.DiskUsed,
			"disk_usage_percent": fmt.Sprintf("%.2f%%", float64(systemMetrics.DiskUsed)/float64(systemMetrics.DiskTotal)*100),
			"os":                 systemMetrics.OS,
			"public_ip":          systemMetrics.PublicIP,
			"uptime_seconds":     systemMetrics.Uptime.Seconds(),
		},
		"database": map[string]interface{}{
			"data_db_size_bytes":   systemMetrics.DataDBSize,
			"data_db_size_mb":      float64(systemMetrics.DataDBSize) / 1024 / 1024,
			"system_db_size_bytes": systemMetrics.SystemDBSize,
			"system_db_size_mb":    float64(systemMetrics.SystemDBSize) / 1024 / 1024,
			"system_db_size_gb":    float64(systemMetrics.SystemDBSize) / 1024 / 1024 / 1024,
		},
		"network": map[string]interface{}{
			"total_download_bytes": systemMetrics.TotalDownload,
			"total_download_mb":    float64(systemMetrics.TotalDownload) / 1024 / 1024,
			"total_download_gb":    float64(systemMetrics.TotalDownload) / 1024 / 1024 / 1024,
			"total_upload_bytes":   systemMetrics.TotalUpload,
			"total_upload_mb":      float64(systemMetrics.TotalUpload) / 1024 / 1024,
			"total_upload_gb":      float64(systemMetrics.TotalUpload) / 1024 / 1024 / 1024,
			"packets_sent":         systemMetrics.PacketsSent,
			"packets_received":     systemMetrics.PacketsReceived,
		},
		"miners": map[string]interface{}{
			"active_count": systemMetrics.ActiveMiners,
			"list":         minersData,
		},
		"pools": poolsData,
	}
	
	minersMutex.RUnlock()
	poolMetricsMtx.RUnlock()
	systemMtx.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleNetworkStats(w http.ResponseWriter, r *http.Request) {
	hoursStr := r.URL.Query().Get("hours")
	hours := 24
	if hoursStr != "" {
		fmt.Sscanf(hoursStr, "%d", &hours)
	}
	
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	
	query := `SELECT timestamp, bytes_download, bytes_upload, packets_sent, packets_received 
		FROM network_traffic WHERE timestamp > ? ORDER BY timestamp ASC`
	
	rows, err := systemDB.Query(query, cutoff)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	
	var stats []map[string]interface{}
	for rows.Next() {
		var timestamp time.Time
		var download, upload, sent, received int64
		
		if err := rows.Scan(&timestamp, &download, &upload, &sent, &received); err != nil {
			continue
		}
		
		stats = append(stats, map[string]interface{}{
			"timestamp":        timestamp.Format(time.RFC3339),
			"bytes_download":   download,
			"bytes_upload":     upload,
			"packets_sent":     sent,
			"packets_received": received,
		})
	}
	
	response := map[string]interface{}{
		"hours":       hours,
		"data_points": len(stats),
		"stats":       stats,
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleSharesStats(w http.ResponseWriter, r *http.Request) {
	wallet := r.URL.Query().Get("wallet")
	hoursStr := r.URL.Query().Get("hours")
	hours := 24
	if hoursStr != "" {
		fmt.Sscanf(hoursStr, "%d", &hours)
	}
	
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	
	var query string
	var rows *sql.Rows
	var err error
	
	if wallet != "" {
		query = `SELECT wallet, miner_name, pool_name, accepted, difficulty, submitted_at 
			FROM shares WHERE wallet = ? AND submitted_at > ? ORDER BY submitted_at DESC LIMIT 1000`
		rows, err = systemDB.Query(query, wallet, cutoff)
	} else {
		query = `SELECT wallet, miner_name, pool_name, accepted, difficulty, submitted_at 
			FROM shares WHERE submitted_at > ? ORDER BY submitted_at DESC LIMIT 1000`
		rows, err = systemDB.Query(query, cutoff)
	}
	
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	
	var shares []map[string]interface{}
	acceptedCount := 0
	rejectedCount := 0
	
	for rows.Next() {
		var w, name, pool string
		var accepted int
		var difficulty float64
		var submitted time.Time
		
		if err := rows.Scan(&w, &name, &pool, &accepted, &difficulty, &submitted); err != nil {
			continue
		}
		
		if accepted == 1 {
			acceptedCount++
		} else {
			rejectedCount++
		}
		
		shares = append(shares, map[string]interface{}{
			"wallet":       w,
			"miner_name":   name,
			"pool":         pool,
			"accepted":     accepted == 1,
			"difficulty":   difficulty,
			"submitted_at": submitted.Format(time.RFC3339),
		})
	}
	
	response := map[string]interface{}{
		"wallet":         wallet,
		"hours":          hours,
		"total_shares":   len(shares),
		"accepted_count": acceptedCount,
		"rejected_count": rejectedCount,
		"acceptance_rate": func() float64 {
			if len(shares) > 0 {
				return float64(acceptedCount) / float64(len(shares)) * 100
			}
			return 0
		}(),
		"shares": shares,
	}
	
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
		bytes_download, bytes_upload, packets_sent, packets_received, 
		current_hashrate, average_hashrate, connected_at, last_seen
		FROM miners WHERE wallet LIKE ?`
	rows, err := dataDB.Query(query, wallet+"%")
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	
	var minersList []map[string]interface{}
	for rows.Next() {
		var (
			dbWallet, minerName, ip, poolName                        string
			sharesAccepted, sharesRejected                           int64
			bytesDownload, bytesUpload, packetsSent, packetsReceived int64
			currentHashrate, averageHashrate                         float64
			connectedAt, lastSeen                                    time.Time
		)
		err := rows.Scan(&dbWallet, &minerName, &ip, &poolName, &sharesAccepted, &sharesRejected,
			&bytesDownload, &bytesUpload, &packetsSent, &packetsReceived,
			&currentHashrate, &averageHashrate, &connectedAt, &lastSeen)
		if err != nil {
			continue
		}
		
		uptime := lastSeen.Sub(connectedAt).Seconds()
		minerData := map[string]interface{}{
			"wallet": dbWallet, "miner_name": minerName, "ip": ip, "pool_name": poolName,
			"shares_accepted": sharesAccepted, "shares_rejected": sharesRejected,
			"bytes_download": bytesDownload, "bytes_upload": bytesUpload,
			"packets_sent": packetsSent, "packets_received": packetsReceived,
			"current_hashrate":     formatHashrate(currentHashrate),
			"average_hashrate":     formatHashrate(averageHashrate),
			"uptime_seconds":       uptime,
			"connected_at":         connectedAt.Format(time.RFC3339),
			"last_seen":            lastSeen.Format(time.RFC3339),
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
				"current_hashrate": formatHashrate(miner.CurrentHashrate),
				"average_hashrate": formatHashrate(miner.AverageHashrate),
				"difficulty":       miner.Difficulty,
				"uptime_seconds":   uptime,
				"connected_at":     miner.ConnectedAt.Format(time.RFC3339),
				"last_seen":        miner.LastSeen.Format(time.RFC3339),
				"status":           "online",
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
	
	fmt.Fprintf(w, "# HELP mining_tunnel_cpu_usage_percent CPU usage percentage\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_cpu_usage_percent gauge\n")
	fmt.Fprintf(w, "mining_tunnel_cpu_usage_percent %.2f\n\n", systemMetrics.CPUUsage)
	
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
	
	fmt.Fprintf(w, "# HELP mining_tunnel_bytes_total Total bytes transferred\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_bytes_total counter\n")
	fmt.Fprintf(w, "mining_tunnel_bytes_total{direction=\"download\"} %d\n", systemMetrics.TotalDownload)
	fmt.Fprintf(w, "mining_tunnel_bytes_total{direction=\"upload\"} %d\n\n", systemMetrics.TotalUpload)
	
	fmt.Fprintf(w, "# HELP mining_tunnel_packets_total Total packets transferred\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_packets_total counter\n")
	fmt.Fprintf(w, "mining_tunnel_packets_total{direction=\"sent\"} %d\n", systemMetrics.PacketsSent)
	fmt.Fprintf(w, "mining_tunnel_packets_total{direction=\"received\"} %d\n\n", systemMetrics.PacketsReceived)
	
	// THÊM POOL METRICS (từ code cũ)
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
	
	fmt.Fprintf(w, "# HELP mining_tunnel_miner_hashrate Miner hashrate in H/s\n")
	fmt.Fprintf(w, "# TYPE mining_tunnel_miner_hashrate gauge\n")
	for _, miner := range miners {
		if miner.Wallet != "" {
			fmt.Fprintf(w, "mining_tunnel_miner_hashrate{wallet=\"%s\",miner=\"%s\",type=\"current\"} %.2f\n",
				miner.Wallet, miner.Name, miner.CurrentHashrate)
			fmt.Fprintf(w, "mining_tunnel_miner_hashrate{wallet=\"%s\",miner=\"%s\",type=\"average\"} %.2f\n",
				miner.Wallet, miner.Name, miner.AverageHashrate)
		}
	}
	fmt.Fprintf(w, "\n")
	
	minersMutex.RUnlock()
	poolMetricsMtx.RUnlock()
	systemMtx.RUnlock()
}
func getCurrentTime() string {
	return time.Now().Format("2006-01-02 15:04:05")
}
func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}
