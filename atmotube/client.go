package atmotube

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

type Client struct {
	adapter    *bluetooth.Adapter
	enableOnce sync.Once
	enableErr  error
}

func NewClient() *Client {
	return &Client{
		adapter: bluetooth.DefaultAdapter,
	}
}

func (c *Client) Enable() error {
	c.enableOnce.Do(func() {
		c.enableErr = c.adapter.Enable()
	})
	return c.enableErr
}

func (c *Client) Scan(ctx context.Context, timeout time.Duration, addressFilter string) ([]DiscoveredDevice, error) {
	if err := c.Enable(); err != nil {
		return nil, err
	}

	filter := strings.ToUpper(strings.TrimSpace(addressFilter))
	results := map[string]DiscoveredDevice{}
	var mu sync.Mutex

	stopTimer := time.AfterFunc(timeout, func() {
		_ = c.adapter.StopScan()
	})
	defer stopTimer.Stop()

	go func() {
		<-ctx.Done()
		_ = c.adapter.StopScan()
	}()

	err := c.adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
		address := strings.ToUpper(result.Address.String())
		if filter != "" && address != filter {
			return
		}

		device := DiscoveredDevice{
			ID:                 address,
			DeviceID:           address,
			Address:            address,
			Name:               result.LocalName(),
			RSSI:               result.RSSI,
			HasAtmotubeService: result.HasServiceUUID(ServiceUUID),
		}

		for _, manufacturer := range result.ManufacturerData() {
			if manufacturer.CompanyID != 0xFFFF {
				continue
			}
			if parsed := parseAdvertisement(manufacturer.Data); parsed != nil {
				device.Broadcast = parsed
				break
			}
		}

		mu.Lock()
		results[address] = device
		mu.Unlock()

		if filter != "" && address == filter {
			_ = adapter.StopScan()
		}
	})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "not scanning") {
		return nil, err
	}

	mu.Lock()
	defer mu.Unlock()
	devices := make([]DiscoveredDevice, 0, len(results))
	for _, device := range results {
		devices = append(devices, device)
	}
	return devices, nil
}

func (c *Client) ReadSnapshot(address string) (Reading, error) {
	if err := c.Enable(); err != nil {
		return Reading{}, err
	}

	targetAddress, err := parseAddress(address)
	if err != nil {
		return Reading{}, err
	}

	device, err := c.adapter.Connect(targetAddress, bluetooth.ConnectionParams{})
	if err != nil {
		return Reading{}, fmt.Errorf("connect %s: %w", address, err)
	}
	defer device.Disconnect()

	services, err := device.DiscoverServices([]bluetooth.UUID{ServiceUUID})
	if err != nil {
		return Reading{}, fmt.Errorf("discover Atmotube service: %w", err)
	}
	if len(services) == 0 {
		return Reading{}, fmt.Errorf("atmotube service not found on %s", address)
	}

	characteristics, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{
		VOCUUID,
		BMEUUID,
		StatusUUID,
		PMUUID,
	})
	if err != nil {
		return Reading{}, fmt.Errorf("discover Atmotube characteristics: %w", err)
	}
	if len(characteristics) < 4 {
		return Reading{}, fmt.Errorf("expected 4 Atmotube characteristics, got %d", len(characteristics))
	}

	vocPayload, err := readCharacteristic(characteristics[0], 4)
	if err != nil {
		return Reading{}, fmt.Errorf("read VOC characteristic: %w", err)
	}
	bmePayload, err := readCharacteristic(characteristics[1], 8)
	if err != nil {
		return Reading{}, fmt.Errorf("read BME characteristic: %w", err)
	}
	statusPayload, err := readCharacteristic(characteristics[2], 2)
	if err != nil {
		return Reading{}, fmt.Errorf("read status characteristic: %w", err)
	}
	pmPayload, err := readCharacteristic(characteristics[3], 12)
	if err != nil {
		return Reading{}, fmt.Errorf("read PM characteristic: %w", err)
	}

	vocPPB, err := parseVOC(vocPayload)
	if err != nil {
		return Reading{}, err
	}
	humidity, temperatureC, pressureMbar, err := parseBME(bmePayload)
	if err != nil {
		return Reading{}, err
	}
	infoByte, batteryPercent, err := parseStatus(statusPayload)
	if err != nil {
		return Reading{}, err
	}
	pm1, pm25, pm4, pm10, err := parsePM(pmPayload)
	if err != nil {
		return Reading{}, err
	}

	return Reading{
		SampledAt:       time.Now().UTC().Format(time.RFC3339),
		VOCPPB:          vocPPB,
		VOCPPM:          float64(vocPPB) / 1000,
		HumidityPercent: humidity,
		TemperatureC:    temperatureC,
		PressureMbar:    pressureMbar,
		BatteryPercent:  batteryPercent,
		StatusByte:      statusPayload[0],
		InfoByte:        infoByte,
		PM1UGM3:         pm1,
		PM25UGM3:        pm25,
		PM4UGM3:         pm4,
		PM10UGM3:        pm10,
	}, nil
}

func parseAddress(value string) (bluetooth.Address, error) {
	mac, err := bluetooth.ParseMAC(strings.TrimSpace(value))
	if err != nil {
		return bluetooth.Address{}, fmt.Errorf("parse bluetooth address %q: %w", value, err)
	}
	return bluetooth.Address{
		MACAddress: bluetooth.MACAddress{
			MAC: mac,
		},
	}, nil
}

func readCharacteristic(characteristic bluetooth.DeviceCharacteristic, size int) ([]byte, error) {
	buffer := make([]byte, size)
	n, err := characteristic.Read(buffer)
	if err != nil {
		return nil, err
	}
	return buffer[:n], nil
}
