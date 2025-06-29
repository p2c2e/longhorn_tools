#!/bin/bash

# Build script for Longhorn Volume Manager
# Builds binaries for macOS, Linux, and Windows

set -e

APP_NAME="lhc"
VERSION=${VERSION:-"dev"}
BUILD_DIR="build"

echo "Building Longhorn Volume Manager v${VERSION}"

# Create build directory
mkdir -p ${BUILD_DIR}

# Build for macOS (Intel)
echo "Building for macOS (Intel)..."
GOOS=darwin GOARCH=amd64 go build -ldflags "-X main.version=${VERSION}" -o ${BUILD_DIR}/${APP_NAME}-darwin-amd64 main.go

# Build for macOS (Apple Silicon)
echo "Building for macOS (Apple Silicon)..."
GOOS=darwin GOARCH=arm64 go build -ldflags "-X main.version=${VERSION}" -o ${BUILD_DIR}/${APP_NAME}-darwin-arm64 main.go

# Build for Linux (Intel)
echo "Building for Linux (Intel)..."
GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=${VERSION}" -o ${BUILD_DIR}/${APP_NAME}-linux-amd64 main.go

# Build for Linux (ARM64)
echo "Building for Linux (ARM64)..."
GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=${VERSION}" -o ${BUILD_DIR}/${APP_NAME}-linux-arm64 main.go

# Build for Windows (Intel)
echo "Building for Windows (Intel)..."
GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=${VERSION}" -o ${BUILD_DIR}/${APP_NAME}-windows-amd64.exe main.go

# Build for Windows (ARM64)
echo "Building for Windows (ARM64)..."
GOOS=windows GOARCH=arm64 go build -ldflags "-X main.version=${VERSION}" -o ${BUILD_DIR}/${APP_NAME}-windows-arm64.exe main.go

echo ""
echo "Build complete! Binaries created in ${BUILD_DIR}/"
ls -la ${BUILD_DIR}/

echo ""
echo "To build for a specific platform only:"
echo "  macOS Intel:     GOOS=darwin GOARCH=amd64 go build -o lhc main.go"
echo "  macOS ARM:       GOOS=darwin GOARCH=arm64 go build -o lhc main.go"
echo "  Linux Intel:     GOOS=linux GOARCH=amd64 go build -o lhc main.go"
echo "  Linux ARM:       GOOS=linux GOARCH=arm64 go build -o lhc main.go"
echo "  Windows Intel:   GOOS=windows GOARCH=amd64 go build -o lhc.exe main.go"
echo "  Windows ARM:     GOOS=windows GOARCH=arm64 go build -o lhc.exe main.go"
