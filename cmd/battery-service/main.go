package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"battery-service/battery"
)

func main() {
	// Parse command line flags
	config := &battery.ServiceConfig{}
	
	// Redis configuration
	flag.StringVar(&config.RedisServerAddress, "redis-server", "127.0.0.1", "Redis server address")
	var redisPort uint
	flag.UintVar(&redisPort, "redis-port", 6379, "Redis server port")
	
	// Battery service configuration
	flag.UintVar(&config.OffUpdateTime, "off-update-time", 1800, "Update time when off in seconds")
	flag.BoolVar(&config.TestMainPower, "test-main-power", false, "Enable main power test mode")
	var heartbeatTimeout uint
	flag.UintVar(&heartbeatTimeout, "heartbeat-timeout", 40, "Heartbeat timeout in seconds")
	var debugMode bool
	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging for detailed NCI/DATA messages")

	// Battery 0 configuration
	flag.StringVar(&config.Batteries[0].DeviceName, "device0", "/dev/pn5xx_i2c0", "Battery 0 NFC device")
	var logLevel0 int
	flag.IntVar(&logLevel0, "log0", 3, "Battery 0 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	config.Batteries[0].LogLevel = logLevel0

	// Battery 1 configuration
	flag.StringVar(&config.Batteries[1].DeviceName, "device1", "/dev/pn5xx_i2c1", "Battery 1 NFC device")
	var logLevel1 int
	flag.IntVar(&logLevel1, "log1", 3, "Battery 1 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	config.Batteries[1].LogLevel = logLevel1

	flag.Parse()

	// Convert uint to uint16 where needed
	config.RedisServerPort = uint16(redisPort)
	config.HeartbeatTimeout = uint16(heartbeatTimeout)

	// Create logger
	logger := log.New(os.Stdout, "", log.LstdFlags)

	// Create battery service
	service, err := battery.NewService(config, logger, debugMode)
	if err != nil {
		logger.Fatalf("Failed to create battery service: %v", err)
	}

	// Start the service
	if err := service.Start(); err != nil {
		logger.Fatalf("Failed to start battery service: %v", err)
	}

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Stop the service
	service.Stop()
} 