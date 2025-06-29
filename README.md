# Longhorn Volume Manager

A command-line tool for managing Longhorn volumes in Kubernetes clusters. This tool provides easy access to list, inspect, download, and copy Longhorn persistent volumes.

## Features

- **List Volumes**: Display all Longhorn volumes with their status, size, and PV binding information
- **View Contents**: Recursively browse the contents of any Longhorn volume
- **Download Volumes**: Export volume data as compressed tar.gz archives
- **Copy Volumes**: Copy data between Longhorn volumes
- **Cleanup**: Remove temporary resources created by the tool

## Prerequisites

- Go 1.19 or later
- Access to a Kubernetes cluster with Longhorn installed
- Valid kubeconfig file or in-cluster configuration

## Installation

### Build from Source

```bash
# Clone the repository
git clone <repository-url>
cd <repository-name>

# Build for your current platform
go build -o lhc main.go

# Or use the build script for multiple platforms
./build.sh
```

### Pre-built Binaries

Check the releases page for pre-built binaries for macOS, Linux, and Windows.

## Usage

```bash
./lhc <command> [flags]
```

### Commands

#### List Volumes
```bash
./lhc list -n <namespace>
```
Lists all Longhorn volumes with their current status, size, and PV binding information.

#### View Volume Contents
```bash
./lhc contents -v <volume-name> -n <namespace> [-s <storage-class>]
```
Displays the directory structure and contents of the specified volume.

#### Download Volume
```bash
./lhc download -v <volume-name> -n <namespace> -o <output-file.tar.gz> [-s <storage-class>]
```
Downloads the entire volume contents as a compressed tar.gz file.

#### Copy Volume
```bash
./lhc copy -s <source-volume> -d <dest-volume> -n <namespace> [-c <storage-class>]
```
Copies all data from the source volume to the destination volume.

#### Cleanup Temporary Resources
```bash
./lhc cleanup -n <namespace>
```
Removes any temporary pods, PVCs, and PVs created by this tool.

### Flags

- `-n, --namespace`: Kubernetes namespace (required for most commands)
- `-v, --volume`: Volume name
- `-s, --source`: Source volume name (for copy command)
- `-d, --dest`: Destination volume name (for copy command)
- `-o, --output`: Output file path (for download command)
- `-c, --storage-class`: Storage class name (optional, defaults to longhorn)

## How It Works

The tool works by:

1. **Volume Discovery**: Uses the Kubernetes API to discover Longhorn volumes via Custom Resources
2. **Pod Access**: Creates temporary pods or uses existing pods that have the volume mounted
3. **Data Operations**: Executes commands inside pods to list, copy, or stream volume data
4. **Cleanup**: Automatically removes temporary resources when operations complete

For volumes not currently in use, the tool creates temporary ReadWriteMany PVs and pods to provide access.

## Configuration

The tool uses standard Kubernetes configuration:

- **In-cluster**: Automatically detects when running inside a Kubernetes pod
- **Kubeconfig**: Uses `~/.kube/config` or the file specified by `KUBECONFIG` environment variable

## Examples

```bash
# List all volumes in the default namespace
./lhc list -n default

# View contents of a specific volume
./lhc contents -v my-data-volume -n production

# Download a volume backup
./lhc download -v database-volume -n production -o database-backup.tar.gz

# Copy data between volumes
./lhc copy -s old-volume -d new-volume -n production

# Clean up temporary resources
./lhc cleanup -n production
```

## Troubleshooting

### Common Issues

1. **Permission Denied**: Ensure your kubeconfig has sufficient permissions to create pods, PVCs, and PVs
2. **Volume Not Found**: Verify the volume name and namespace are correct
3. **Longhorn Not Available**: Ensure Longhorn is installed and the `longhorn-system` namespace exists

### Debug Mode

Set the `KUBECONFIG` environment variable to specify a custom kubeconfig file:

```bash
export KUBECONFIG=/path/to/your/kubeconfig
./lhc list -n default
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests if applicable
5. Submit a pull request

## License

MIT 
