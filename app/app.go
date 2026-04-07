package app

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	runtimekit "github.com/piphi-network/piphi-runtime-kit-go"
	"github.com/piphi-network/piphi-runtime-kit-go/adapters"

	"github.com/KelvinSan/piphi-network-atmotube/atmotube"
)

const integrationID = "piphi-network-atmotube-pro"

type App struct {
	runtime       *runtimekit.RuntimeContext
	registry      *runtimekit.RuntimeRegistry[atmotube.DeviceEntry, atmotube.Reading, map[string]any]
	telemetry     *runtimekit.TelemetryClient
	coordinator   *runtimekit.ConfigSyncCoordinator[atmotube.DeviceConfig]
	ble           BLEClient
	enablePolling bool

	mu      sync.Mutex
	pollers map[string]context.CancelFunc
}

type BLEClient interface {
	Scan(ctx context.Context, timeout time.Duration, addressFilter string) ([]atmotube.DiscoveredDevice, error)
	ReadSnapshot(address string) (atmotube.Reading, error)
}

func New() *App {
	return NewWithOptions(atmotube.NewClient(), true)
}

func NewWithOptions(ble BLEClient, enablePolling bool) *App {
	runtime := runtimekit.NewRuntimeContext()
	return &App{
		runtime:       runtime,
		registry:      runtimekit.NewRuntimeRegistry[atmotube.DeviceEntry, atmotube.Reading, map[string]any](100),
		telemetry:     runtimekit.NewTelemetryClient(runtime.ProcessState, "", 0),
		coordinator:   runtimekit.NewConfigSyncCoordinator[atmotube.DeviceConfig](runtime.ProcessState),
		ble:           ble,
		enablePolling: enablePolling,
		pollers:       map[string]context.CancelFunc{},
	}
}

func (a *App) Router() *gin.Engine {
	router := gin.Default()
	router.GET("/health", a.handleHealth)
	router.GET("/diagnostics", a.handleDiagnostics)
	router.GET("/ui", a.handleUISchema)
	router.GET("/discover", a.handleDiscover)
	router.POST("/discover", a.handleDiscover)
	router.POST("/config", a.handleConfig)
	router.POST("/config/sync", a.handleConfigSync)
	router.POST("/deconfigure", a.handleDeconfigure)
	router.GET("/state", a.handleState)
	router.GET("/events", a.handleEvents)
	router.GET("/entities", a.handleEntities)
	return router
}

func (a *App) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, runtimekit.BuildRuntimeHealthResponse(
		a.runtime,
		map[string]any{
			"id":      integrationID,
			"name":    "Atmotube Pro (BLE)",
			"version": "0.1.0",
		},
		map[string]any{
			"active_configs": len(a.registry.IDs()),
		},
	))
}

func (a *App) handleDiagnostics(c *gin.Context) {
	c.JSON(http.StatusOK, runtimekit.BuildRuntimeDiagnosticsResponse(
		a.runtime,
		map[string]any{
			"id":      integrationID,
			"name":    "Atmotube Pro (BLE)",
			"version": "0.1.0",
		},
		map[string]any{
			"active_config_ids":  a.registry.IDs(),
			"recent_event_count": len(a.registry.RecentEvents()),
			"bluetooth_enabled":  true,
		},
	))
}

func (a *App) handleUISchema(c *gin.Context) {
	c.JSON(http.StatusOK, map[string]any{
		"schema": map[string]any{
			"title":       "Atmotube Pro Bluetooth Setup",
			"description": "Connect PiPhi to an Atmotube Pro by Bluetooth address and choose how often PiPhi should poll the device.",
			"type":        "object",
			"required":    []string{"address"},
			"properties": map[string]any{
				"id": map[string]any{
					"type":  "string",
					"title": "Config ID",
				},
				"address": map[string]any{
					"type":        "string",
					"title":       "Bluetooth Address",
					"description": "Atmotube Pro Bluetooth MAC address, for example 11:22:33:AA:BB:CC.",
					"examples":    []string{"11:22:33:AA:BB:CC"},
				},
				"alias": map[string]any{
					"type":        "string",
					"title":       "Alias",
					"description": "Optional name shown in PiPhi dashboards and setup flows.",
				},
				"poll_interval_seconds": map[string]any{
					"type":        "integer",
					"title":       "Poll Interval Seconds",
					"description": "How often PiPhi should reconnect to the Atmotube Pro and read fresh sensor values.",
					"default":     atmotube.DefaultPollIntervalSeconds,
					"minimum":     5,
				},
			},
		},
		"uiSchema": map[string]any{
			"address": map[string]any{
				"autocomplete": "off",
				"placeholder":  "11:22:33:AA:BB:CC",
				"help":         "Use the Bluetooth MAC address shown during discovery or from the device label.",
			},
			"alias": map[string]any{
				"placeholder": "Bedroom Atmotube",
			},
			"poll_interval_seconds": map[string]any{
				"help": "A longer interval uses less battery; 30 seconds is a good default.",
			},
		},
	})
}

func (a *App) handleDiscover(c *gin.Context) {
	inputs := map[string]any{}
	if c.Request.Method == http.MethodPost {
		var payload runtimekit.IntegrationDiscoveryRequest
		if err := c.ShouldBindJSON(&payload); err == nil && payload.Inputs != nil {
			inputs = payload.Inputs
		}
	}
	normalizedInputs := runtimekit.NormalizeDiscoveryInputs(inputs)
	log.Println(runtimekit.FormatDiscoveryAttemptLog(normalizedInputs))

	timeout := 6 * time.Second
	if rawTimeout, ok := normalizedInputs["scan_seconds"].(float64); ok && rawTimeout > 0 {
		timeout = time.Duration(rawTimeout) * time.Second
	}
	addressFilter, _ := normalizedInputs["address"].(string)

	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	devices, err := a.ble.Scan(ctx, timeout, addressFilter)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, runtimekit.BuildDiscoveryResponse(devices))
}

func (a *App) handleConfig(c *gin.Context) {
	var payload atmotube.DeviceConfig
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	payload = payload.Normalize()
	a.syncRuntimeAuth(c, payload.ContainerID)
	log.Println(runtimekit.FormatConfigApplyLog(map[string]any{
		"id":             payload.ID,
		"container_id":   payload.ContainerID,
		"integration_id": payload.IntegrationID,
		"address":        payload.Address,
		"alias":          payload.Alias,
	}))

	entry, err := a.applyConfig(payload)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, runtimekit.BuildConfigApplyResponse(
		entry.ConfigID,
		entry.ContainerID,
		map[string]any{
			"address": entry.Address,
			"alias":   entry.Alias,
		},
	))
}

func (a *App) handleConfigSync(c *gin.Context) {
	var payload runtimekit.RuntimeConfigSnapshot[atmotube.DeviceConfig]
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	a.syncRuntimeAuth(c, payload.ContainerID)

	for index := range payload.Configs {
		payload.Configs[index] = payload.Configs[index].Normalize()
	}

	response, err := a.coordinator.ApplySnapshot(
		payload,
		a.registry.IDs(),
		func(config atmotube.DeviceConfig) error {
			_, applyErr := a.applyConfig(config)
			return applyErr
		},
		func(configID string) (bool, error) {
			return a.removeConfig(configID), nil
		},
		func() []string {
			return a.registry.IDs()
		},
	)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, response)
}

func (a *App) handleDeconfigure(c *gin.Context) {
	var payload struct {
		ConfigID string `json:"config_id"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	removed := a.removeConfig(payload.ConfigID)
	c.JSON(http.StatusOK, runtimekit.BuildConfigRemoveResponse(
		payload.ConfigID,
		removed,
		map[string]any{
			"remaining_configs": len(a.registry.IDs()),
		},
	))
}

func (a *App) handleState(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"entries":         a.registry.EntriesSnapshot(),
		"state_snapshots": a.registry.StateSnapshots(),
	})
}

func (a *App) handleEvents(c *gin.Context) {
	limit := 50
	if rawLimit := c.Query("limit"); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	events := a.registry.RecentEvents()
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	c.JSON(http.StatusOK, runtimekit.BuildEventListResponse(events))
}

func (a *App) handleEntities(c *gin.Context) {
	entities := make([]map[string]any, 0, len(a.registry.IDs()))
	for _, entryID := range a.registry.IDs() {
		entry, ok := a.registry.Get(entryID)
		if !ok {
			continue
		}
		entities = append(entities, map[string]any{
			"id":           entry.DeviceID,
			"config_id":    entry.ConfigID,
			"address":      entry.Address,
			"alias":        entry.Alias,
			"latest_state": entry.LatestState,
		})
	}
	c.JSON(http.StatusOK, gin.H{"entities": entities})
}

func (a *App) syncRuntimeAuth(c *gin.Context, payloadContainerID string) {
	parsed := adapters.SyncRuntimeAuthFromGinContext(a.runtime, c, payloadContainerID)
	log.Println(adapters.FormatRuntimeAuthSyncLogFromGinContext(c, payloadContainerID))
	_ = parsed
}

func (a *App) applyConfig(config atmotube.DeviceConfig) (atmotube.DeviceEntry, error) {
	config = config.Normalize()
	reading, err := a.ble.ReadSnapshot(config.Address)
	if err != nil {
		return atmotube.DeviceEntry{}, err
	}

	entry := atmotube.DeviceEntry{
		ConfigID:      config.ID,
		DeviceID:      config.DeviceID,
		ContainerID:   config.ContainerID,
		IntegrationID: firstNonEmpty(config.IntegrationID, integrationID),
		Address:       config.Address,
		Alias:         config.Alias,
		Config:        config,
		LatestState:   reading,
		Metadata: map[string]any{
			"poll_interval_seconds": config.PollIntervalSeconds,
		},
	}
	a.registry.Set(config.ID, entry)
	a.registry.UpdateState(config.ID, reading)
	a.appendLocalEvent("atmotube.config.applied", entry, map[string]any{
		"address": config.Address,
		"alias":   config.Alias,
	})
	if a.enablePolling {
		a.startPoller(entry)
	}
	return entry, nil
}

func (a *App) removeConfig(configID string) bool {
	a.mu.Lock()
	cancel, ok := a.pollers[configID]
	if ok {
		delete(a.pollers, configID)
	}
	a.mu.Unlock()
	if ok {
		cancel()
	}

	entry, removed := a.registry.Remove(configID)
	if removed {
		a.appendLocalEvent("atmotube.config.removed", entry, nil)
	}
	return removed
}

func (a *App) startPoller(entry atmotube.DeviceEntry) {
	a.mu.Lock()
	if existing, ok := a.pollers[entry.ConfigID]; ok {
		existing()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.pollers[entry.ConfigID] = cancel
	a.mu.Unlock()

	runtimekit.CreateTrackedTask(a.runtime.ProcessState, func() {
		ticker := time.NewTicker(entry.Config.PollInterval())
		defer ticker.Stop()

		for {
			a.pollOnce(entry)

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (a *App) pollOnce(entry atmotube.DeviceEntry) {
	reading, err := a.ble.ReadSnapshot(entry.Address)
	if err != nil {
		a.appendLocalEvent("atmotube.poll.error", entry, map[string]any{
			"error": err.Error(),
		})
		return
	}

	entry.LatestState = reading
	a.registry.Set(entry.ConfigID, entry)
	a.registry.UpdateState(entry.ConfigID, reading)

	runtimekit.ScheduleTelemetryDelivery(
		a.runtime.ProcessState,
		a.telemetry,
		a.runtime.Auth,
		runtimekit.TelemetryPayload{
			DeviceID:      entry.DeviceID,
			ContainerID:   entry.ContainerID,
			IntegrationID: entry.IntegrationID,
			Metrics: map[string]any{
				"voc_ppb":          reading.VOCPPB,
				"voc_ppm":          reading.VOCPPM,
				"humidity_percent": reading.HumidityPercent,
				"temperature_c":    reading.TemperatureC,
				"pressure_mbar":    reading.PressureMbar,
				"battery_percent":  reading.BatteryPercent,
				"pm1_ugm3":         reading.PM1UGM3,
				"pm25_ugm3":        reading.PM25UGM3,
				"pm4_ugm3":         reading.PM4UGM3,
				"pm10_ugm3":        reading.PM10UGM3,
			},
			Units: map[string]any{
				"voc_ppb":          "ppb",
				"voc_ppm":          "ppm",
				"humidity_percent": "%",
				"temperature_c":    "C",
				"pressure_mbar":    "mbar",
				"battery_percent":  "%",
				"pm1_ugm3":         "ug/m3",
				"pm25_ugm3":        "ug/m3",
				"pm4_ugm3":         "ug/m3",
				"pm10_ugm3":        "ug/m3",
			},
			Timestamp: reading.SampledAt,
		},
	)
}

func (a *App) appendLocalEvent(eventType string, entry atmotube.DeviceEntry, payload map[string]any) {
	event := runtimekit.BuildLocalEventRecord(map[string]any{
		"event_type":     eventType,
		"source":         "piphi-network-atmotube",
		"severity":       "info",
		"device_id":      entry.DeviceID,
		"config_id":      entry.ConfigID,
		"container_id":   entry.ContainerID,
		"integration_id": entry.IntegrationID,
		"payload":        payload,
	})
	a.registry.AppendEvent(event)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
