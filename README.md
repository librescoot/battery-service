# MDB Battery Service

[![CC BY-NC-SA 4.0][cc-by-nc-sa-shield]][cc-by-nc-sa]

The MDB Battery Service is a critical component responsible for managing and monitoring battery systems through NFC communication. This service handles real-time battery state management, safety monitoring, and communication with the Redis messaging system.

## Features

- Dual battery monitoring system
- Real-time battery state management
- NFC-based communication with batteries
- Temperature monitoring and safety controls
- Redis-based messaging system for component communication
- Configurable update intervals for different battery states
- Automatic battery presence detection

## Dependencies

- `github.com/redis/go-redis/v9` - Redis client for Go
- `golang.org/x/sys` - System calls and primitives
- NFC Hardware Abstraction Layer (HAL) for PN7150 NFC controller

## System Architecture

The service is built around two main components:
- **Battery Service**: Core service managing multiple battery readers
- **Battery Reader**: Individual reader instances managing NFC communication with batteries

### Key Components

- **NFC Communication**: Handles low-level communication with battery NFC tags
- **State Management**: Tracks battery presence, temperature, and operational states
- **Redis Integration**: Manages communication with other system components
- **Safety Monitoring**: Monitors temperature limits and battery states
- **Configuration System**: Flexible configuration for different deployment scenarios

## Building and Running

To build the service:

```bash
make build
```

To run the service:

```bash
./battery-service [options]
```

### Command Line Options

- `--redis-server`: Redis server address (default: "127.0.0.1")
- `--redis-port`: Redis server port (default: 6379)
- `--off-update-time`: Update time when off in seconds (default: 1800)
- `--heartbeat-timeout`: Heartbeat timeout in seconds (default: 40)
- `--device0`: Battery 0 NFC device path (default: "/dev/pn5xx_i2c0")
- `--device1`: Battery 1 NFC device path (default: "/dev/pn5xx_i2c1")
- `--log0`: Battery 0 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)
- `--log1`: Battery 1 log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)

## Safety Features

The service implements several safety features:
- Temperature state monitoring (Cold/Normal/Hot states)
- Automatic battery presence detection
- Heartbeat monitoring
- Multiple retry mechanisms for reliable communication
- Comprehensive error logging and reporting

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This work is licensed under a
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png
[cc-by-nc-sa-shield]: https://img.shields.io/badge/License-CC%20BY--NC--SA%204.0-lightgrey.svg

---

Made with ❤️ by the LibreScoot community
