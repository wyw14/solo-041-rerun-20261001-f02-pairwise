package process

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/wyw14/cry-97/internal/model"
)

type DeviceBus struct {
	mu       sync.RWMutex
	commands map[string]model.DeviceCommand
}

func NewDeviceBus() *DeviceBus {
	return &DeviceBus{commands: make(map[string]model.DeviceCommand)}
}

func (b *DeviceBus) Publish(ctx context.Context, command model.DeviceCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if command.DeviceID == "" || command.Generation == 0 {
		return errors.New("device command is incomplete")
	}
	b.mu.Lock()
	current, exists := b.commands[command.DeviceID]
	if exists && command.Generation < current.Generation {
		b.mu.Unlock()
		return model.ErrCommandStale
	}
	b.commands[command.DeviceID] = command
	b.mu.Unlock()
	return nil
}

func (b *DeviceBus) All() []model.DeviceCommand {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]model.DeviceCommand, 0, len(b.commands))
	for _, command := range b.commands {
		result = append(result, command)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IssuedAt.Before(result[j].IssuedAt) })
	return result
}

func (b *DeviceBus) StopLine(lineID model.LineID, generation uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, command := range b.commands {
		if command.LineID != lineID {
			continue
		}
		command.Generation = generation
		command.Action = "stop"
		command.Setpoint = 0
		b.commands[id] = command
	}
}
