package model

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type DeviceKind string

const (
	DeviceBlower DeviceKind = "blower"
	DevicePump   DeviceKind = "pump"
	DeviceValve  DeviceKind = "valve"
	DeviceDoser  DeviceKind = "doser"
)

// ErrCommandStale 表示命令代际早于总线上当前命令（例如处理线已急停推进代际）。
var ErrCommandStale = errors.New("device command generation is stale")

type DeviceCommand struct {
	ID         uuid.UUID         `json:"id"`
	LineID     LineID            `json:"line_id"`
	BatchID    uuid.UUID         `json:"batch_id"`
	DeviceID   string            `json:"device_id"`
	Kind       DeviceKind        `json:"kind"`
	Generation uint64            `json:"generation"`
	Action     string            `json:"action"`
	Setpoint   float64           `json:"setpoint,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	IssuedAt   time.Time         `json:"issued_at"`
}

func NewDeviceCommand(lineID LineID, batchID uuid.UUID, deviceID string, kind DeviceKind, generation uint64, action string, now time.Time) (DeviceCommand, error) {
	if lineID == "" || batchID == uuid.Nil || deviceID == "" || generation == 0 || action == "" {
		return DeviceCommand{}, errors.New("device command identity is incomplete")
	}
	return DeviceCommand{
		ID: uuid.New(), LineID: lineID, BatchID: batchID, DeviceID: deviceID,
		Kind: kind, Generation: generation, Action: action, IssuedAt: now.UTC(),
		Metadata: make(map[string]string),
	}, nil
}
