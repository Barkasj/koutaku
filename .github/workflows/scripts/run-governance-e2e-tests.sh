#!/usr/bin/env bash
set -euo pipefail

# Run Governance E2E Tests
# This script builds Koutaku, starts it with the governance test config,
# runs the governance tests, and cleans up.
#
# Usage: ./run-governance-e2e-tests.sh

echo "🛡️ Starting Governance E2E Tests..."

# Get the root directory of the repo
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Configuration
KOUTAKU_PORT=8080
KOUTAKU_HOST="localhost"
KOUTAKU_URL="http://${KOUTAKU_HOST}:${KOUTAKU_PORT}"
APP_DIR="tests/governance"
KOUTAKU_BINARY="tmp/koutaku-http"
KOUTAKU_PID_FILE="/tmp/koutaku-governance-test.pid"
KOUTAKU_LOG_FILE="/tmp/koutaku-governance-test.log"
MAX_STARTUP_WAIT=30 # seconds

# Cleanup function to ensure Koutaku is stopped
cleanup() {
  local exit_code=$?
  echo ""
  echo -e "${YELLOW}🧹 Cleaning up...${NC}"
  
  # Stop Koutaku if running
  if [ -f "$KOUTAKU_PID_FILE" ]; then
    KOUTAKU_PID=$(cat "$KOUTAKU_PID_FILE")
    if ps -p "$KOUTAKU_PID" > /dev/null 2>&1; then
      echo -e "${CYAN}Stopping Koutaku (PID: $KOUTAKU_PID)...${NC}"
      kill "$KOUTAKU_PID" 2>/dev/null || true
      sleep 2
      # Force kill if still running
      if ps -p "$KOUTAKU_PID" > /dev/null 2>&1; then
        echo -e "${YELLOW}Force killing Koutaku...${NC}"
        kill -9 "$KOUTAKU_PID" 2>/dev/null || true
      fi
    fi
    rm -f "$KOUTAKU_PID_FILE"
  fi
  
  # Clean up log file
  if [ -f "$KOUTAKU_LOG_FILE" ]; then
    echo -e "${CYAN}Koutaku logs saved to: $KOUTAKU_LOG_FILE${NC}"
  fi
  
  # Clean up test database
  if [ -f "data/governance-test.db" ]; then
    echo -e "${CYAN}Cleaning up test database...${NC}"
    rm -f "data/governance-test.db"
  fi
  
  if [ $exit_code -eq 0 ]; then
    echo -e "${GREEN}✅ Cleanup complete${NC}"
  else
    echo -e "${RED}❌ Cleanup complete (tests failed)${NC}"
  fi
  
  exit $exit_code
}

# Set up trap to cleanup on exit
trap cleanup EXIT INT TERM

# Step 1: Validate prerequisites
echo -e "${CYAN}📋 Step 1: Validating prerequisites...${NC}"

if [ ! -d "$APP_DIR" ]; then
  echo -e "${RED}❌ App directory not found: $APP_DIR${NC}"
  exit 1
fi

if [ ! -f "$APP_DIR/config.json" ]; then
  echo -e "${RED}❌ Config file not found: $APP_DIR/config.json${NC}"
  exit 1
fi

# Check required environment variables (OPENAI_API_KEY, ANTHROPIC_API_KEY, OPENROUTER_API_KEY)
if [ -z "${OPENAI_API_KEY:-}" ] || [ -z "${ANTHROPIC_API_KEY:-}" ] || [ -z "${OPENROUTER_API_KEY:-}" ]; then
  echo -e "${RED}❌ Required environment variables are not set${NC}"
  echo -e "${YELLOW}Set them with: export OPENAI_API_KEY='sk-...'${NC}"
  echo -e "${YELLOW}Set them with: export ANTHROPIC_API_KEY='sk-...'${NC}"
  echo -e "${YELLOW}Set them with: export OPENROUTER_API_KEY='sk-...'${NC}"
  exit 1
fi
echo -e "${GREEN}✅ Prerequisites validated${NC}"

# Step 2: Build Koutaku
echo ""
echo -e "${CYAN}📦 Step 2: Building Koutaku...${NC}"

# Use make to build with LOCAL=1 to use the workspace (go.work)
# This ensures we test the local governance plugin code, not the published version
if ! make build LOCAL=1; then
  echo -e "${RED}❌ Failed to build Koutaku${NC}"
  exit 1
fi

if [ ! -f "$KOUTAKU_BINARY" ]; then
  echo -e "${RED}❌ Koutaku binary not found at: $KOUTAKU_BINARY${NC}"
  exit 1
fi

echo -e "${GREEN}✅ Koutaku built successfully${NC}"

# Step 3: Start Koutaku in background
echo ""
echo -e "${CYAN}🚀 Step 3: Starting Koutaku server...${NC}"

# Ensure data directory exists for SQLite database
mkdir -p data

# Start Koutaku in background
echo -e "${YELLOW}Starting Koutaku on ${KOUTAKU_URL}...${NC}"
"$KOUTAKU_BINARY" -app-dir "$APP_DIR" -port "$KOUTAKU_PORT" -host "$KOUTAKU_HOST" > "$KOUTAKU_LOG_FILE" 2>&1 &
KOUTAKU_PID=$!
echo "$KOUTAKU_PID" > "$KOUTAKU_PID_FILE"

echo -e "${CYAN}Koutaku started with PID: $KOUTAKU_PID${NC}"

# Step 4: Wait for Koutaku to be ready
echo ""
echo -e "${CYAN}⏳ Step 4: Waiting for Koutaku to be ready...${NC}"

WAIT_COUNT=0
until curl -sf "${KOUTAKU_URL}/health" > /dev/null 2>&1; do
  if [ $WAIT_COUNT -ge $MAX_STARTUP_WAIT ]; then
    echo -e "${RED}❌ Koutaku failed to start within ${MAX_STARTUP_WAIT} seconds${NC}"
    echo -e "${YELLOW}Last 50 lines of Koutaku logs:${NC}"
    tail -n 50 "$KOUTAKU_LOG_FILE" || true
    exit 1
  fi
  
  # Check if process is still running
  if ! ps -p "$KOUTAKU_PID" > /dev/null 2>&1; then
    echo -e "${RED}❌ Koutaku process died${NC}"
    echo -e "${YELLOW}Koutaku logs:${NC}"
    cat "$KOUTAKU_LOG_FILE" || true
    exit 1
  fi
  
  WAIT_COUNT=$((WAIT_COUNT + 1))
  echo -e "${YELLOW}Waiting for Koutaku... ($WAIT_COUNT/${MAX_STARTUP_WAIT})${NC}"
  sleep 1
done

echo -e "${GREEN}✅ Koutaku is ready and responding${NC}"

# Step 5: Run governance tests
echo ""
echo -e "${CYAN}🧪 Step 5: Running governance tests...${NC}"

cd tests/governance

# Run tests with go test (disable workspace to avoid module conflicts)
echo -e "${YELLOW}Running go test in tests/governance...${NC}"

# Run tests with verbose output and timeout
# Use GOWORK=off to disable the workspace file and test the module independently
# Use -count=1 to disable test cache
GOWORK=off go test -v -timeout 10m -count=1 ./...
TEST_EXIT_CODE=$?
if [ $TEST_EXIT_CODE -ne 0 ]; then
  echo -e "${RED}❌ Governance tests failed (exit code: $TEST_EXIT_CODE)${NC}"
else
  echo -e "${GREEN}✅ All governance tests passed${NC}"
fi

cd "$REPO_ROOT"

# Step 6: Report results
echo ""
if [ $TEST_EXIT_CODE -eq 0 ]; then
  echo -e "${GREEN}═══════════════════════════════════════════════════════${NC}"
  echo -e "${GREEN}✅ Governance E2E Tests PASSED${NC}"
  echo -e "${GREEN}═══════════════════════════════════════════════════════${NC}"
else
  echo -e "${RED}═══════════════════════════════════════════════════════${NC}"
  echo -e "${RED}❌ Governance E2E Tests FAILED${NC}"
  echo -e "${RED}═══════════════════════════════════════════════════════${NC}"
  echo ""
  echo -e "${YELLOW}Check logs at: $KOUTAKU_LOG_FILE${NC}"
fi

exit $TEST_EXIT_CODE
