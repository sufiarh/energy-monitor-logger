package main

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goburrow/modbus"
)

/* ================= CONFIG ================= */

const (
	PORT              = "/dev/ttyUSB0"
	SLAVEID           = 1
	BAUDRATE          = 9600
	POLL_INT          = 1 * time.Second
	CSV_FLUSH         = 10 * time.Minute
	MAX_CONSEC_ERRORS = 3
	RECONNECT_DELAY   = 3 * time.Second
	ENERGY_STATE_FILE = "energy_state.json"
	CONFIG_FILE       = "config.json"
	HISTORY_CSV       = "history.csv"
	HISTORY_JSONL     = "history.jsonl"
	EVENTS_JSONL      = "events.jsonl"
)

/* ================= APP CONFIG (tariff & daya) ================= */

type AppConfig struct {
	Tariff float64 `json:"tariff"` // Rp per kWh
	MaxVA  float64 `json:"max_va"` // Daya terpasang dalam VA
}

var (
	appConfig   AppConfig
	appConfigMu sync.RWMutex
)

func defaultConfig() AppConfig {
	return AppConfig{Tariff: 900, MaxVA: 7700}
}

func loadAppConfig() AppConfig {
	b, err := os.ReadFile(CONFIG_FILE)
	if err != nil {
		cfg := defaultConfig()
		saveAppConfig(cfg)
		return cfg
	}
	var cfg AppConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaultConfig()
	}
	if cfg.Tariff <= 0 {
		cfg.Tariff = 1444
	}
	if cfg.MaxVA <= 0 {
		cfg.MaxVA = 7700
	}
	return cfg
}

func saveAppConfig(cfg AppConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(CONFIG_FILE, b, 0644)
}

/* ================= AUTH ================= */

const (
	PASSWORD       = "asiap1234"
	SESSION_COOKIE = "pzem_session"
	SESSION_TTL    = 24 * time.Hour
)

type session struct {
	createdAt time.Time
}

var (
	sessions   = make(map[string]session)
	sessionsMu sync.Mutex
)

func newSessionToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func isValidSession(token string) bool {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	s, ok := sessions[token]
	if !ok {
		return false
	}
	if time.Since(s.createdAt) > SESSION_TTL {
		delete(sessions, token)
		return false
	}
	return true
}

func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login.html" || r.URL.Path == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(SESSION_COOKIE)
		if err != nil || !isValidSession(cookie.Value) {
			if len(r.URL.Path) > 4 && r.URL.Path[:5] == "/api/" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/login.html", http.StatusFound)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if body.Password != PASSWORD {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	token := newSessionToken()
	sessionsMu.Lock()
	sessions[token] = session{createdAt: time.Now()}
	sessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     SESSION_COOKIE,
		Value:    token,
		Path:     "/",
		MaxAge:   int(SESSION_TTL.Seconds()),
		       HttpOnly: true,
		       SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SESSION_COOKIE); err == nil {
		sessionsMu.Lock()
		delete(sessions, cookie.Value)
		sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: SESSION_COOKIE, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login.html", http.StatusFound)
}

/* ================= DATA TYPES ================= */

type Metrics struct {
	Timestamp    time.Time `json:"ts"`
	Voltage      float64   `json:"voltage"`
	Current      float64   `json:"current"`
	Power        float64   `json:"power"`
	Energy       float64   `json:"energy"`
	Frequency    float64   `json:"frequency"`
	PF           float64   `json:"pf"`
	SystemStatus string    `json:"system_status"`
}

type EnergyState struct {
	Date        string  `json:"date"`
	EnergyStart float64 `json:"energy_start"`
}

type Event struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"`
	Message   string `json:"msg"`
}

type DeviceStatus struct {
	Connected  bool      `json:"connected"`
	Status     string    `json:"status"`
	LastSeen   time.Time `json:"last_seen"`
	ErrorCount int       `json:"error_count"`
}

type APIResponse struct {
	Metrics     Metrics      `json:"metrics"`
	Device      DeviceStatus `json:"device"`
	EnergyToday float64      `json:"energy_today"`
	Config      AppConfig    `json:"config"`
}

/* ================= GLOBAL ================= */

var (
	currentData Metrics
	mu          sync.RWMutex

	deviceStatus DeviceStatus
	statusMu     sync.RWMutex

	csvBuffer []Metrics
	csvMu     sync.Mutex

	energyState EnergyState
	energyMu    sync.RWMutex

	lastAlarm string

	resetEnergyPending int32

	lastLoggedMinute int  // Track menit terakhir yang sudah di-log (untuk tepat di :00, :10, :20, dst)
csvLogMu         sync.Mutex
)

/* ================= THRESHOLD ================= */

const (
	MIN_VOLTAGE_V   = 198.0
	MAX_VOLTAGE_V   = 231.0
	MIN_FREQUENCY_HZ = 49.5
	MAX_FREQUENCY_HZ = 50.5
)

func determineStatus(voltage, frequency float64) string {
	switch {
		case voltage < MIN_VOLTAGE_V:
			return "UNDER_VOLTAGE"
		case voltage > MAX_VOLTAGE_V:
			return "OVER_VOLTAGE"
		case frequency < MIN_FREQUENCY_HZ:
			return "UNDER_FREQUENCY"
		case frequency > MAX_FREQUENCY_HZ:
			return "OVER_FREQUENCY"
		default:
			return "NORMAL"
	}
}

/* ================= MODBUS UTIL ================= */

func reg16(buf []byte, reg int) uint16 {
	i := reg * 2
	return uint16(buf[i])<<8 | uint16(buf[i+1])
}

/* ================= ENERGY STATE ================= */

func loadEnergyState() EnergyState {
	b, err := os.ReadFile(ENERGY_STATE_FILE)
	if err != nil {
		return EnergyState{Date: time.Now().Format("2006-01-02"), EnergyStart: 0}
	}
	var s EnergyState
	json.Unmarshal(b, &s)
	return s
}

func saveEnergyState(s EnergyState) {
	b, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(ENERGY_STATE_FILE, b, 0644)
}

func checkDailyRollover(currentEnergy float64) {
	today := time.Now().Format("2006-01-02")
	energyMu.Lock()
	defer energyMu.Unlock()
	if today != energyState.Date {
		log.Printf("[ENERGY] Daily rollover %s → %s, start=%.4f kWh", energyState.Date, today, currentEnergy)
		energyState = EnergyState{Date: today, EnergyStart: currentEnergy}
		saveEnergyState(energyState)
		appendEvent(EVENTS_JSONL, Event{
			Timestamp: time.Now().Format(time.RFC3339),
			    Level:     "INFO",
			    Message:   "Reset counter energi harian pada tengah malam",
		})
	}
}

func getEnergyToday(currentEnergy float64) float64 {
	energyMu.RLock()
	defer energyMu.RUnlock()
	v := currentEnergy - energyState.EnergyStart
	if v < 0 {
		return 0
	}
	return v
}

/* ================= LOGGING ================= */

func appendJSONL(path string, m Metrics) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(m)
	f.Write(append(b, '\n'))
}

func appendEvent(path string, e Event) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	f.Write(append(b, '\n'))
}

// immediateFlushCSV: flush langsung tanpa menunggu 10 menit, dipanggil saat ada gangguan
func immediateFlushCSV(m Metrics) {
	csvMu.Lock()
	csvBuffer = append(csvBuffer, m)
	data := csvBuffer
	csvBuffer = nil
	csvMu.Unlock()

	flushCSV(HISTORY_CSV, data)
	log.Printf("[CSV] Immediate flush: %s — %.1fW [%s]", m.Timestamp.Format(time.RFC3339), m.Power, m.SystemStatus)
}

func flushCSV(path string, data []Metrics) {
	if len(data) == 0 {
		return
	}
	_, err := os.Stat(path)
	newFile := os.IsNotExist(err)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if newFile {
		w.Write([]string{"timestamp", "voltage", "current", "power", "energy_kwh", "frequency", "pf", "systemstatus"})
	}
	for _, m := range data {
		w.Write([]string{
			m.Timestamp.Format(time.RFC3339),
			fmt.Sprintf("%.1f", m.Voltage),
			fmt.Sprintf("%.3f", m.Current),
			fmt.Sprintf("%.1f", m.Power),
			fmt.Sprintf("%.4f", m.Energy),
			fmt.Sprintf("%.1f", m.Frequency),
			fmt.Sprintf("%.2f", m.PF),
			m.SystemStatus,
		})
	}
	w.Flush()
}

/* ================= DEVICE STATUS ================= */

func setConnected() {
	statusMu.Lock()
	defer statusMu.Unlock()
	deviceStatus = DeviceStatus{Connected: true, Status: "NORMAL", LastSeen: time.Now()}
}

func setDisconnected(errCount int) {
	statusMu.Lock()
	defer statusMu.Unlock()
	deviceStatus.Connected = false
	deviceStatus.Status = "DEVICE DISCONNECTED"
	deviceStatus.ErrorCount = errCount
}

/* ================= PZEM ENERGY RESET ================= */

func sendEnergyResetCommand() error {
	f, err := os.OpenFile(PORT, os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		return fmt.Errorf("open serial: %w", err)
	}
	defer f.Close()
	_, err = f.Write([]byte{0x01, 0x42, 0x80, 0x11})
	if err != nil {
		return fmt.Errorf("write reset cmd: %w", err)
	}
	time.Sleep(200 * time.Millisecond)
	log.Println("[PZEM] Energy reset command sent")
	return nil
}

/* ================= PZEM LOOP ================= */

func pzemLoop() {
	consecErrors := 0

	for {
		handler := modbus.NewRTUClientHandler(PORT)
		handler.BaudRate = BAUDRATE
		handler.DataBits = 8
		handler.Parity = "N"
		handler.StopBits = 1
		handler.SlaveId = SLAVEID
		handler.Timeout = time.Second

		if err := handler.Connect(); err != nil {
			log.Printf("[PZEM] Connect failed: %v — retry in %v", err, RECONNECT_DELAY)
			setDisconnected(consecErrors)
			appendEvent(EVENTS_JSONL, Event{
				Timestamp: time.Now().Format(time.RFC3339),
				    Level:     "ERROR",
				    Message:   "Perangkat terputus: " + err.Error(),
			})
			time.Sleep(RECONNECT_DELAY)
			continue
		}

		log.Println("[PZEM] Connected")
		consecErrors = 0
		setConnected()
		appendEvent(EVENTS_JSONL, Event{
			Timestamp: time.Now().Format(time.RFC3339),
			    Level:     "INFO",
			    Message:   "Perangkat terhubung",
		})

		client := modbus.NewClient(handler)

		for {
			if atomic.CompareAndSwapInt32(&resetEnergyPending, 1, 0) {
				log.Println("[PZEM] Processing energy reset")
				handler.Close()
				if err := sendEnergyResetCommand(); err != nil {
					log.Printf("[PZEM] Energy reset error: %v", err)
					appendEvent(EVENTS_JSONL, Event{
						Timestamp: time.Now().Format(time.RFC3339),
						    Level:     "ERROR",
						    Message:   "Reset energi gagal: " + err.Error(),
					})
				} else {
					energyMu.Lock()
					energyState = EnergyState{Date: time.Now().Format("2006-01-02"), EnergyStart: 0}
					saveEnergyState(energyState)
					energyMu.Unlock()
					appendEvent(EVENTS_JSONL, Event{
						Timestamp: time.Now().Format(time.RFC3339),
						    Level:     "INFO",
						    Message:   "Reset energi berhasil",
					})
				}
				time.Sleep(RECONNECT_DELAY)
				break
			}

			buf, err := client.ReadInputRegisters(0, 10)
			if err != nil {
				consecErrors++
				log.Printf("[PZEM] Read error #%d: %v", consecErrors, err)
				setDisconnected(consecErrors)
				if consecErrors >= MAX_CONSEC_ERRORS {
					log.Printf("[PZEM] %d consecutive errors — reconnecting", consecErrors)
					handler.Close()
					appendEvent(EVENTS_JSONL, Event{
						Timestamp: time.Now().Format(time.RFC3339),
						    Level:     "WARNING",
						    Message:   fmt.Sprintf("Mencoba reconnect setelah %d error berturut-turut", consecErrors),
					})
					time.Sleep(RECONNECT_DELAY)
					break
				}
				time.Sleep(time.Second)
				continue
			}

			consecErrors = 0
			setConnected()

			v    := float64(reg16(buf, 0)) / 10.0
			iRaw := uint32(reg16(buf, 1)) | uint32(reg16(buf, 2))<<16
			i    := float64(iRaw) / 1000.0
			pRaw := uint32(reg16(buf, 3)) | uint32(reg16(buf, 4))<<16
			p    := float64(pRaw) / 10.0
			eRaw := uint32(reg16(buf, 5)) | uint32(reg16(buf, 6))<<16
			e    := float64(eRaw) / 1000.0
			f    := float64(reg16(buf, 7)) / 10.0
			pf   := float64(reg16(buf, 8)) / 100.0

			status := determineStatus(v, f)

			m := Metrics{
				Timestamp:    time.Now(),
				Voltage:      v,
				Current:      i,
				Power:        p,
				Energy:       e,
				Frequency:    f,
				PF:           pf,
				SystemStatus: status,
			}

			checkDailyRollover(e)

			mu.Lock()
			currentData = m
			mu.Unlock()

			appendJSONL(HISTORY_JSONL, m)

			// CSV: simpan tepat di menit kelipatan 10 (:00, :10, :20, dst) jika NORMAL,
			// atau langsung simpan jika ABNORMAL
			currentMinute := m.Timestamp.Minute()

			if status != "NORMAL" {
				// ABNORMAL: langsung flush immediate
				immediateFlushCSV(m)
				csvLogMu.Lock()
				lastLoggedMinute = currentMinute
				csvLogMu.Unlock()
			} else if currentMinute%10 == 0 {
				// NORMAL: cek apakah menit kelipatan 10 dan belum di-log di menit ini
				csvLogMu.Lock()
				shouldLog := (currentMinute != lastLoggedMinute)
				if shouldLog {
					lastLoggedMinute = currentMinute
				}
				csvLogMu.Unlock()

				if shouldLog {
					csvMu.Lock()
					csvBuffer = append(csvBuffer, m)
					csvMu.Unlock()
				}
			}
			// Selain itu, skip (tidak disimpan ke CSV)

			if status != lastAlarm {
				reason := "Operasi Normal"
				switch status {
					case "UNDER_VOLTAGE":
						reason = "Under Voltage (< 198V)"
					case "OVER_VOLTAGE":
						reason = "Over Voltage (> 231V)"
					case "UNDER_FREQUENCY":
						reason = "Under Frequency (< 49.5Hz)"
					case "OVER_FREQUENCY":
						reason = "Over Frequency (> 50.5Hz)"
				}
				appendEvent(EVENTS_JSONL, Event{
					Timestamp: time.Now().Format(time.RFC3339),
					    Level:     status,
					    Message:   reason,
				})
				lastAlarm = status
			}

			time.Sleep(POLL_INT)
		}
	}
}

/* ================= API HANDLERS ================= */

func handleRealtime(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	m := currentData
	mu.RUnlock()

	statusMu.RLock()
	s := deviceStatus
	statusMu.RUnlock()

	appConfigMu.RLock()
	cfg := appConfig
	appConfigMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(APIResponse{
		Metrics:     m,
		Device:      s,
		EnergyToday: getEnergyToday(m.Energy),
				  Config:      cfg,
	})
}

func handleGetConfig(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	cfg := appConfig
	appConfigMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

func handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var body AppConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if body.Tariff <= 0 || body.MaxVA <= 0 {
		http.Error(w, "Nilai tarif dan daya harus lebih dari 0", http.StatusBadRequest)
		return
	}
	appConfigMu.Lock()
	appConfig = body
	appConfigMu.Unlock()

	if err := saveAppConfig(body); err != nil {
		http.Error(w, "Gagal menyimpan konfigurasi", http.StatusInternalServerError)
		return
	}
	log.Printf("[CONFIG] Updated: tariff=%.0f, max_va=%.0f", body.Tariff, body.MaxVA)
	appendEvent(EVENTS_JSONL, Event{
		Timestamp: time.Now().Format(time.RFC3339),
		    Level:     "INFO",
		    Message:   fmt.Sprintf("Konfigurasi diperbarui: tarif=Rp%.0f/kWh, daya=%.0fVA", body.Tariff, body.MaxVA),
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleExport(w http.ResponseWriter, r *http.Request) {
	csvMu.Lock()
	pending := csvBuffer
	csvBuffer = nil
	csvMu.Unlock()
	flushCSV(HISTORY_CSV, pending)

	f, err := os.Open(HISTORY_CSV)
	if err != nil {
		http.Error(w, "Belum ada data untuk di-export", http.StatusNotFound)
		return
	}
	defer f.Close()

	filename := fmt.Sprintf("pzem_export_%s.csv", time.Now().Format("20060102_150405"))
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	http.ServeContent(w, r, filename, time.Now(), f)
}

func handleResetEnergy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	statusMu.RLock()
	connected := deviceStatus.Connected
	statusMu.RUnlock()
	if !connected {
		http.Error(w, "Perangkat tidak terhubung", http.StatusServiceUnavailable)
		return
	}
	atomic.StoreInt32(&resetEnergyPending, 1)
	log.Println("[API] Energy reset queued")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
}

func handleResetCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	csvMu.Lock()
	csvBuffer = nil
	csvMu.Unlock()

	if err := os.Remove(HISTORY_CSV); err != nil && !os.IsNotExist(err) {
		http.Error(w, "Gagal menghapus CSV", http.StatusInternalServerError)
		return
	}
	log.Println("[API] history.csv reset")
	appendEvent(EVENTS_JSONL, Event{
		Timestamp: time.Now().Format(time.RFC3339),
		    Level:     "INFO",
		    Message:   "history.csv direset secara manual",
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

/* ================= MAIN ================= */

func main() {
	// Init tracking variable untuk CSV logging
	lastLoggedMinute = -1

	appConfig = loadAppConfig()
	log.Printf("[CONFIG] Loaded: tariff=%.0f, max_va=%.0f", appConfig.Tariff, appConfig.MaxVA)

	energyState = loadEnergyState()
	log.Printf("[ENERGY] Loaded: date=%s, start=%.4f kWh", energyState.Date, energyState.EnergyStart)

	go pzemLoop()

	go func() {
		ticker := time.NewTicker(CSV_FLUSH)
		defer ticker.Stop()
		for range ticker.C {
			csvMu.Lock()
			data := csvBuffer
			csvBuffer = nil
			csvMu.Unlock()
			if len(data) > 0 {
				flushCSV(HISTORY_CSV, data)
				log.Printf("[CSV] Safety flush: %d records (scheduled)", len(data))
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/realtime", handleRealtime)
	mux.HandleFunc("/api/metrics", handleRealtime)
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
			case http.MethodGet:
				handleGetConfig(w, r)
			case http.MethodPost:
				handleSetConfig(w, r)
			default:
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/export", handleExport)
	mux.HandleFunc("/api/reset-energy", handleResetEnergy)
	mux.HandleFunc("/api/reset-csv", handleResetCSV)
	mux.Handle("/", noCacheMiddleware(http.FileServer(http.Dir("web"))))

	log.Println("Dashboard berjalan di :8080")
	log.Fatal(http.ListenAndServe(":8080", authMiddleware(mux)))
}
