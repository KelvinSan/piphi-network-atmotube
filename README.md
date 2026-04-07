# piphi-network-atmotube

PiPhi runtime integration for the Atmotube Pro using Go, Gin, the local
`piphi-runtime-kit-go`, and `tinygo.org/x/bluetooth`.

## What it does

- scans nearby BLE devices and filters for the Atmotube Pro service
- discovers Atmotube Pro devices over Bluetooth Low Energy
- connects to a configured Atmotube Pro by Bluetooth MAC address
- reads the Atmotube GATT characteristics for:
  - VOC
  - humidity / temperature / pressure
  - battery and status
  - PM1 / PM2.5 / PM4 / PM10
- keeps the latest state in the runtime registry
- queues telemetry back to PiPhi Core through the Go runtime kit

## Runtime routes

- `GET /health`
- `GET /diagnostics`
- `GET /ui`
- `GET /discover`
- `POST /discover`
- `POST /config`
- `POST /config/sync`
- `POST /deconfigure`
- `GET /state`
- `GET /events`
- `GET /entities`

## Configuration payload

`POST /config`

```json
{
  "id": "bedroom-atmotube",
  "device_id": "bedroom-atmotube",
  "container_id": "runtime-123",
  "integration_id": "piphi-network-atmotube-pro",
  "address": "11:22:33:AA:BB:CC",
  "alias": "Bedroom Atmotube",
  "poll_interval_seconds": 30
}
```

## Local development

This project expects:

- a working BLE stack on the host
- BlueZ on Linux for scanning / connecting
- the local Go runtime kit at `../piphi-runtime-kit-go`

When you have the Go toolchain available, run:

```bash
go mod tidy
gofmt -w .
go test ./...
```

## Device contract references

- Atmotube Bluetooth API: https://support.atmotube.com/en/articles/10364981-bluetooth-api
- Atmotube Bluetooth API specification: https://support.atmotube.com/en/articles/10449987-bluetooth-api-specification
- TinyGo Bluetooth package: https://pkg.go.dev/tinygo.org/x/bluetooth
