[private]
default:
    @just --list

# Build the live service binary.
build:
    bash launch/service.sh build

# Run the test suite with the race detector.
test:
    go test ./... -race -count=1

# Test the staging-only provenance build helper (offline).
test-build:
    python3 -m unittest discover -s launch -p 'test_*.py'

# Build a stamped artifact outside live paths; never deploy/restart.
artifact output:
    python3 launch/build.py --output {{quote(output)}}

# Run Go's static analyzer.
vet:
    go vet ./...

# Update Go module dependencies.
tidy:
    go mod tidy

# Build and install the launch agent.
install:
    bash launch/service.sh install

# Rebuild and restart the launch agent.
reload:
    bash launch/service.sh reload

# Remove the launch agent while preserving binary and config.
uninstall:
    bash launch/service.sh uninstall

# Follow the service log.
logs:
    bash launch/service.sh logs

# Show the launch agent status.
status:
    bash launch/service.sh status
