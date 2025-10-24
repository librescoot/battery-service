package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"battery-service/battery"
)

// Build-time variables set by linker
var (
	gitRevision = "unknown"
	buildTime   = "unknown"
)

func main() {
	config := &battery.ServiceConfig{}

	// Version flag
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "Show version information")

	// Service log level
	var serviceLogLevel int
	flag.IntVar(&serviceLogLevel, "log", 3, "Service log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")

	// Redis configuration
	flag.StringVar(&config.RedisServerAddress, "redis-server", "127.0.0.1", "Redis server address")
	var redisPort uint
	flag.UintVar(&redisPort, "redis-port", 6379, "Redis server port")

	flag.BoolVar(&config.TestMainPower, "test-main-power", false, "Enable main power test mode")
	var heartbeatTimeout, offUpdateTime uint
	flag.UintVar(&heartbeatTimeout, "heartbeat-timeout", 40, "Heartbeat timeout for standby mode in seconds")
	flag.UintVar(&offUpdateTime, "off-update-time", 1800, "Update time when disabled in seconds (30 minutes)")
	var debugMode bool
	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging for detailed NCI/DATA messages")
	flag.BoolVar(&config.DangerouslyIgnoreSeatbox, "dangerously-ignore-seatbox", false, "Keep active batteries active when seatbox opens (DANGEROUS)")

	var device0, device1 string
	var logLevel0, logLevel1 int
	var battery1Active bool
	flag.StringVar(&device0, "device0", "/dev/pn5xx_i2c0", "Battery 0 NFC device")
	flag.IntVar(&logLevel0, "log0", 3, "Battery 0 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	flag.StringVar(&device1, "device1", "/dev/pn5xx_i2c1", "Battery 1 NFC device")
	flag.IntVar(&logLevel1, "log1", 3, "Battery 1 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	flag.BoolVar(&battery1Active, "battery1-active", false, "Enable battery 1 as active in addition to battery 0 (default: inactive)")

	flag.Parse()

	// Show version and exit if requested
	if showVersion {
		fmt.Printf("battery-service version %s (built %s)\n", gitRevision, buildTime)
		os.Exit(0)
	}

	// Convert uint to uint16 and Duration where needed
	config.RedisServerPort = uint16(redisPort)
	config.HeartbeatTimeout = time.Duration(heartbeatTimeout) * time.Second
	config.OffUpdateTime = time.Duration(offUpdateTime) * time.Second

	var stdLogger *log.Logger
	if os.Getenv("INVOCATION_ID") != "" {
		stdLogger = log.New(os.Stdout, "", 0)
	} else {
		stdLogger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.Lmsgprefix)
	}

	logger := battery.NewLogger(stdLogger, battery.LogLevel(serviceLogLevel))

	batteryConfig := &battery.BatteryConfiguration{
		Readers: []battery.BatteryReaderConfig{
			{
				Index:      0,
				Role:       battery.BatteryRoleActive,
				Enabled:    true,
				DeviceName: device0,
				LogLevel:   logLevel0,
			},
			{
				Index:      1,
				Role:       battery.BatteryRoleInactive,
				Enabled:    true,
				DeviceName: device1,
				LogLevel:   logLevel1,
			},
		},
	}

	if battery1Active {
		batteryConfig.Readers[1].Role = battery.BatteryRoleActive
	}

	// Log version information at startup
	logger.Infof("Battery service v2 starting (git: %s, built: %s)", gitRevision, buildTime)

	// Create battery service
	service, err := battery.NewService(config, batteryConfig, stdLogger, battery.LogLevel(serviceLogLevel), debugMode)
	if err != nil {
		logger.Fatalf("Failed to create battery service: %v", err)
	}

	if err := service.Start(); err != nil {
		logger.Fatalf("Failed to start battery service: %v", err)
	}

	logger.Infof("Battery service v2 started with %d readers", len(batteryConfig.Readers))

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logger.Infof("Shutting down battery service v2...")

	service.Stop()
}
