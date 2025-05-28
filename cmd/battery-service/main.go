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
	flag.BoolVar(&config.TestMainPower, "test-main-power", false, "Enable main power test mode")
	var debugMode bool
	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging for detailed NCI/DATA messages")

	// Battery configuration
	var device0, device1 string
	var logLevel0, logLevel1 int
	var battery1Active bool
	flag.StringVar(&device0, "device0", "/dev/pn5xx_i2c0", "Battery 0 NFC device")
	flag.IntVar(&logLevel0, "log0", 3, "Battery 0 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	flag.StringVar(&device1, "device1", "/dev/pn5xx_i2c1", "Battery 1 NFC device")
	flag.IntVar(&logLevel1, "log1", 3, "Battery 1 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	flag.BoolVar(&battery1Active, "battery1-active", false, "Set battery 1 as active (default: inactive)")

	flag.Parse()

	// Convert uint to uint16 where needed
	config.RedisServerPort = uint16(redisPort)

	// Create logger
	logger := log.New(os.Stdout, "", log.LstdFlags)

	// Create battery configuration
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

	// Set battery 1 role based on flag
	if battery1Active {
		batteryConfig.Readers[1].Role = battery.BatteryRoleActive
	}

	// Create battery service
	service, err := battery.NewService(config, batteryConfig, logger, debugMode)
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