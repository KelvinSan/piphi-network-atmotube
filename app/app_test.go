package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/KelvinSan/piphi-network-atmotube/atmotube"
)

type fakeBLEClient struct {
	discovered []atmotube.DiscoveredDevice
	reading    atmotube.Reading
	scanErr    error
	readErr    error
}

func (f *fakeBLEClient) Scan(ctx context.Context, timeout time.Duration, addressFilter string) ([]atmotube.DiscoveredDevice, error) {
	return f.discovered, f.scanErr
}

func (f *fakeBLEClient) ReadSnapshot(address string) (atmotube.Reading, error) {
	return f.reading, f.readErr
}

func TestDiscoverReturnsBLEDevices(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fakeBLE := &fakeBLEClient{
		discovered: []atmotube.DiscoveredDevice{
			{
				ID:                 "11:22:33:AA:BB:CC",
				DeviceID:           "11:22:33:AA:BB:CC",
				Address:            "11:22:33:AA:BB:CC",
				Name:               "Atmotube Pro",
				RSSI:               -48,
				HasAtmotubeService: true,
				Broadcast: map[string]any{
					"voc_ppb": 220,
				},
			},
		},
	}

	app := NewWithOptions(fakeBLE, false)
	router := app.Router()

	request := httptest.NewRequest(http.MethodPost, "/discover", bytes.NewReader([]byte(`{"inputs":{"address":"11:22:33:AA:BB:CC"}}`)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		Devices []atmotube.DiscoveredDevice `json:"devices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(response.Devices) != 1 {
		t.Fatalf("expected one discovered device, got %d", len(response.Devices))
	}
	if response.Devices[0].Address != "11:22:33:AA:BB:CC" {
		t.Fatalf("unexpected discovered address: %s", response.Devices[0].Address)
	}
	if !response.Devices[0].HasAtmotubeService {
		t.Fatalf("expected discovered device to advertise the Atmotube service")
	}
}

func TestConfigSyncAppliesAndRemovesConfigs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fakeBLE := &fakeBLEClient{
		reading: atmotube.Reading{
			SampledAt:       "2026-04-06T20:00:00Z",
			VOCPPB:          120,
			VOCPPM:          0.12,
			HumidityPercent: 45.2,
			TemperatureC:    22.4,
			PressureMbar:    1013.2,
			BatteryPercent:  87,
			StatusByte:      1,
			InfoByte:        2,
			PM1UGM3:         1.1,
			PM25UGM3:        2.3,
			PM4UGM3:         3.4,
			PM10UGM3:        4.5,
		},
	}

	app := NewWithOptions(fakeBLE, false)
	router := app.Router()

	firstConfig := atmotube.DeviceConfig{
		Address: "11:22:33:AA:BB:CC",
		Alias:   "Bedroom",
	}
	firstConfig.ID = "bedroom"
	firstConfig.DeviceID = "bedroom"
	firstConfig.ContainerID = "container-1"
	firstConfig.IntegrationID = "integration-1"

	_, err := app.applyConfig(firstConfig)
	if err != nil {
		t.Fatalf("applyConfig failed: %v", err)
	}

	secondConfig := atmotube.DeviceConfig{
		Address:             "AA:BB:CC:DD:EE:FF",
		Alias:               "Office",
		PollIntervalSeconds: 45,
	}
	secondConfig.ID = "office"
	secondConfig.DeviceID = "office"
	secondConfig.ContainerID = "container-1"
	secondConfig.IntegrationID = "integration-1"

	generation := 7
	payload := map[string]any{
		"container_id":   "container-1",
		"integration_id": "integration-1",
		"generation":     generation,
		"configs":        []atmotube.DeviceConfig{secondConfig},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/config/sync", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Container-Id", "container-1")
	request.Header.Set("X-PiPhi-Integration-Token", "test-token")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		OK               bool     `json:"ok"`
		AppliedConfigIDs []string `json:"applied_config_ids"`
		RemovedConfigIDs []string `json:"removed_config_ids"`
		SkippedConfigIDs []string `json:"skipped_config_ids"`
		Generation       *int     `json:"generation"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if !response.OK {
		t.Fatalf("expected ok response")
	}
	if len(response.AppliedConfigIDs) != 1 || response.AppliedConfigIDs[0] != "office" {
		t.Fatalf("unexpected applied config ids: %#v", response.AppliedConfigIDs)
	}
	if len(response.RemovedConfigIDs) != 1 || response.RemovedConfigIDs[0] != "bedroom" {
		t.Fatalf("unexpected removed config ids: %#v", response.RemovedConfigIDs)
	}
	if len(response.SkippedConfigIDs) != 0 {
		t.Fatalf("unexpected skipped config ids: %#v", response.SkippedConfigIDs)
	}
	if response.Generation == nil || *response.Generation != generation {
		t.Fatalf("unexpected generation: %#v", response.Generation)
	}

	if _, ok := app.registry.Get("bedroom"); ok {
		t.Fatalf("expected stale config to be removed from registry")
	}
	entry, ok := app.registry.Get("office")
	if !ok {
		t.Fatalf("expected synced config to exist in registry")
	}
	if entry.Address != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("unexpected synced address: %s", entry.Address)
	}
	if len(app.registry.RecentEvents()) == 0 {
		t.Fatalf("expected config lifecycle events to be recorded")
	}
}
