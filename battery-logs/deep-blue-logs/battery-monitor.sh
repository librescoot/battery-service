#!/bin/bash

# Battery and system state monitor script
# Saves librescoot-battery logs when anomalies are detected

REDIS_CMD="redis-cli"
LOG_DIR="/data/battery-logs"
CHECK_INTERVAL=5
LOG_LOOKBACK=90

# Ensure log directory exists
mkdir -p "$LOG_DIR"

# Log startup to journal
echo "[BATTERY-MONITOR] Starting battery state monitor service"
echo "[BATTERY-MONITOR] Log directory: $LOG_DIR"
echo "[BATTERY-MONITOR] Check interval: ${CHECK_INTERVAL}s, Log lookback: ${LOG_LOOKBACK}s"

# Previous state variables for change detection
prev_seatbox=""
prev_battery_present=""
prev_battery_state=""
prev_vehicle_state=""
prev_aux_charge=""

# Service and debounce tracking
service_was_running=true
last_anomaly=""
anomaly_logged=false

log_anomaly() {
    local reason="$1"
    local timestamp=$(date +"%Y%m%dT%H%M%S")
    local log_file="${LOG_DIR}/battery-log-${timestamp}-${reason}.log"

    # Log to journal (stdout)
    echo "[BATTERY-MONITOR] Anomaly detected: $reason"
    echo "[BATTERY-MONITOR] Capturing logs to: $log_file"

    # Also log to file
    echo "[$(date)] Anomaly detected: $reason" >> "${LOG_DIR}/monitor.log"
    echo "Capturing logs to: $log_file" >> "${LOG_DIR}/monitor.log"

    # Capture last 90 seconds of battery service logs
    journalctl -u librescoot-battery --since "${LOG_LOOKBACK} seconds ago" --no-pager > "$log_file" 2>&1

    # Also capture current redis state
    {
        echo "=== Anomaly: $reason ==="
        echo "=== Timestamp: $(date) ==="
        echo "=== Redis State ==="
        echo "battery:0 present: $($REDIS_CMD hget battery:0 present)"
        echo "battery:0 state: $($REDIS_CMD hget battery:0 state)"
        echo "vehicle seatbox:lock: $($REDIS_CMD hget vehicle seatbox:lock)"
        echo "vehicle state: $($REDIS_CMD hget vehicle state)"
        echo "aux-battery charge-status: $($REDIS_CMD hget aux-battery charge-status)"
        echo "=== Journal Logs ==="
    } >> "$log_file.state"
}

# Function to check and log anomalies with debouncing
check_and_log_anomaly() {
    local current_anomaly="$1"

    # If no current anomaly, clear debounce state
    if [ -z "$current_anomaly" ]; then
        if [ -n "$last_anomaly" ]; then
            echo "[BATTERY-MONITOR] Anomaly cleared: $last_anomaly"
            echo "[$(date)] Anomaly cleared: $last_anomaly" >> "${LOG_DIR}/monitor.log"
        fi
        last_anomaly=""
        anomaly_logged=false
        return
    fi

    # If this is a new anomaly or we haven't logged this one yet
    if [ "$current_anomaly" != "$last_anomaly" ] || [ "$anomaly_logged" = false ]; then
        log_anomaly "$current_anomaly"
        last_anomaly="$current_anomaly"
        anomaly_logged=true
    fi
}

echo "[$(date)] Battery monitor started" >> "${LOG_DIR}/monitor.log"
echo "[BATTERY-MONITOR] Monitoring started successfully - checking for anomalies every ${CHECK_INTERVAL}s"

while true; do
    # Check if battery service is running
    if pidof battery-service > /dev/null; then
        service_running=true
        if [ "$service_was_running" = false ]; then
            echo "[BATTERY-MONITOR] Battery service is back up"
            echo "[$(date)] Battery service is back up" >> "${LOG_DIR}/monitor.log"
        fi
    else
        service_running=false
        if [ "$service_was_running" = true ]; then
            echo "[BATTERY-MONITOR] Battery service stopped running"
            echo "[$(date)] Battery service stopped running" >> "${LOG_DIR}/monitor.log"
        fi
    fi
    service_was_running=$service_running

    # Skip anomaly checks if service is not running
    if [ "$service_running" = false ]; then
        sleep $CHECK_INTERVAL
        continue
    fi

    # Read current states
    battery_present=$($REDIS_CMD hget battery:0 present 2>/dev/null)
    battery_state=$($REDIS_CMD hget battery:0 state 2>/dev/null)
    seatbox=$($REDIS_CMD hget vehicle seatbox:lock 2>/dev/null)
    vehicle_state=$($REDIS_CMD hget vehicle state 2>/dev/null)
    aux_charge=$($REDIS_CMD hget aux-battery charge-status 2>/dev/null)

    # Determine current anomaly state
    current_anomaly=""

    # Check condition 1: seatbox closed but battery not present or not active
    if [ "$seatbox" = "closed" ]; then
        if [ "$battery_present" = "false" ] || [ "$battery_present" = "" ]; then
            current_anomaly="seatbox-closed-battery-not-present"
        elif [ "$battery_state" != "active" ]; then
            current_anomaly="seatbox-closed-battery-not-active"
        fi
    fi

    # Check condition 3: ready-to-drive but battery not present or not active (higher priority)
    if [ "$vehicle_state" = "ready-to-drive" ]; then
        if [ "$battery_present" = "false" ] || [ "$battery_present" = "" ]; then
            current_anomaly="ready-to-drive-battery-not-present"
        elif [ "$battery_state" != "active" ]; then
            current_anomaly="ready-to-drive-battery-not-active"
        fi
    fi

    # Check condition 4: battery active but aux not charging
    if [ -z "$current_anomaly" ] && [ "$battery_state" = "active" ] && [ "$aux_charge" = "not-charging" ]; then
        current_anomaly="battery-active-aux-not-charging"
    fi

    # Apply debounced logging for ongoing state anomalies
    check_and_log_anomaly "$current_anomaly"

    # Check condition 2: seatbox state change (immediate logging, no debounce)
    if [ -n "$prev_seatbox" ] && [ "$prev_seatbox" != "$seatbox" ]; then
        if [ "$prev_seatbox" = "closed" ] && [ "$seatbox" = "open" ]; then
            log_anomaly "seatbox-opened"
        elif [ "$prev_seatbox" = "open" ] && [ "$seatbox" = "closed" ]; then
            log_anomaly "seatbox-closed"
        fi
    fi

    # Update previous states
    prev_seatbox="$seatbox"
    prev_battery_present="$battery_present"
    prev_battery_state="$battery_state"
    prev_vehicle_state="$vehicle_state"
    prev_aux_charge="$aux_charge"

    sleep $CHECK_INTERVAL
done