#!/bin/bash
# hint-client.sh - Send workload hints to the Smart Fan Controller
#
# Usage:
#   hint-client.sh start whisper high     # Signal high GPU load starting
#   hint-client.sh stop whisper           # Signal load complete
#   hint-client.sh status                 # Check controller status

CONTROLLER_URL="${SMART_FAN_URL:-http://localhost:8086}"

# Bearer token for the mutating endpoints (override / hint). Set API_TOKEN to
# match api.token in the controller config. When empty, the controller only
# accepts mutating requests from the local host (loopback).
API_TOKEN="${API_TOKEN:-}"

# AUTH_ARGS carries the Authorization header for mutating requests when a token
# is configured; it stays empty for read-only calls and loopback-only setups.
AUTH_ARGS=()
if [ -n "$API_TOKEN" ]; then
    AUTH_ARGS=(-H "Authorization: Bearer $API_TOKEN")
fi

case "$1" in
    start)
        SOURCE="$2"
        INTENSITY="${3:-medium}"
        DURATION="${4:-0}"
        
        if [ -z "$SOURCE" ]; then
            echo "Usage: $0 start <source> [intensity] [duration_seconds]"
            exit 1
        fi
        
        curl -s -X POST "$CONTROLLER_URL/api/hint" \
            "${AUTH_ARGS[@]}" \
            -H "Content-Type: application/json" \
            -d "{
                \"type\": \"gpu_load\",
                \"action\": \"start\",
                \"intensity\": \"$INTENSITY\",
                \"duration_estimate\": $DURATION,
                \"source\": \"$SOURCE\"
            }" | jq .
        ;;
        
    stop)
        SOURCE="$2"
        
        if [ -z "$SOURCE" ]; then
            echo "Usage: $0 stop <source>"
            exit 1
        fi
        
        curl -s -X POST "$CONTROLLER_URL/api/hint" \
            "${AUTH_ARGS[@]}" \
            -H "Content-Type: application/json" \
            -d "{
                \"type\": \"gpu_load\",
                \"action\": \"stop\",
                \"source\": \"$SOURCE\"
            }" | jq .
        ;;
        
    status)
        curl -s "$CONTROLLER_URL/api/status" | jq .
        ;;
        
    override)
        SPEED="$2"
        DURATION="${3:-0}"
        REASON="${4:-manual}"
        
        if [ -z "$SPEED" ]; then
            echo "Usage: $0 override <speed> [duration_seconds] [reason]"
            exit 1
        fi
        
        curl -s -X POST "$CONTROLLER_URL/api/override" \
            "${AUTH_ARGS[@]}" \
            -H "Content-Type: application/json" \
            -d "{
                \"speed\": $SPEED,
                \"duration\": $DURATION,
                \"reason\": \"$REASON\"
            }" | jq .
        ;;
        
    clear-override)
        curl -s -X DELETE "$CONTROLLER_URL/api/override" \
            "${AUTH_ARGS[@]}" | jq .
        ;;
        
    *)
        echo "Smart Fan Controller Hint Client"
        echo ""
        echo "Usage: $0 <command> [args...]"
        echo ""
        echo "Commands:"
        echo "  start <source> [intensity] [duration]  - Signal workload starting"
        echo "  stop <source>                          - Signal workload complete"
        echo "  status                                 - Get controller status"
        echo "  override <speed> [duration] [reason]   - Set manual fan speed"
        echo "  clear-override                         - Clear manual override"
        echo ""
        echo "Intensity: low, medium, high"
        echo ""
        echo "Environment:"
        echo "  SMART_FAN_URL   Controller base URL (default http://localhost:8086)"
        echo "  API_TOKEN       Bearer token for override/hint (required off-host)"
        echo ""
        echo "Examples:"
        echo "  $0 start whisper high 300"
        echo "  $0 stop whisper"
        echo "  $0 override 50 60 'testing'"
        exit 1
        ;;
esac
