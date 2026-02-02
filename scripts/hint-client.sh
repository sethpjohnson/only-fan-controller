#!/bin/bash
# hint-client.sh - Send workload hints to the Smart Fan Controller
#
# Usage:
#   hint-client.sh start whisper high     # Signal high GPU load starting
#   hint-client.sh stop whisper           # Signal load complete
#   hint-client.sh status                 # Check controller status

CONTROLLER_URL="${SMART_FAN_URL:-http://localhost:8086}"

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
            -H "Content-Type: application/json" \
            -d "{
                \"speed\": $SPEED,
                \"duration\": $DURATION,
                \"reason\": \"$REASON\"
            }" | jq .
        ;;
        
    clear-override)
        curl -s -X DELETE "$CONTROLLER_URL/api/override" | jq .
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
        echo "Examples:"
        echo "  $0 start whisper high 300"
        echo "  $0 stop whisper"
        echo "  $0 override 50 60 'testing'"
        exit 1
        ;;
esac
