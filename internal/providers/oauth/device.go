package oauth

import (
	"context"
	"errors"
	"time"
)

var (
	ErrDeviceDenied  = errors.New("device authorization denied")
	ErrDeviceExpired = errors.New("device authorization expired")
	ErrDeviceTimeout = errors.New("device authorization timed out")
)

type DeviceStatus string

const (
	DeviceSuccess  DeviceStatus = "success"
	DevicePending  DeviceStatus = "pending"
	DeviceSlowDown DeviceStatus = "slow_down"
	DeviceDenied   DeviceStatus = "denied"
	DeviceExpired  DeviceStatus = "expired"
)

type DevicePollResult struct {
	Status DeviceStatus
	Token  TokenResponse
	Err    error
}

// PollDeviceCode performs provider-neutral device polling. The callback is
// responsible for translating provider responses into DevicePollResult.
func PollDeviceCode(ctx context.Context, expiresAt time.Time, interval time.Duration, poll func(context.Context) DevicePollResult) (TokenResponse, error) {
	if poll == nil {
		return TokenResponse{}, errors.New("device poll function is required")
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		if !expiresAt.IsZero() && !time.Now().Before(expiresAt) {
			return TokenResponse{}, ErrDeviceExpired
		}
		result := poll(ctx)
		if result.Err != nil {
			return TokenResponse{}, result.Err
		}
		switch result.Status {
		case DeviceSuccess:
			return result.Token, nil
		case DeviceDenied:
			return TokenResponse{}, ErrDeviceDenied
		case DeviceExpired:
			return TokenResponse{}, ErrDeviceExpired
		case DevicePending, DeviceSlowDown:
			if result.Status == DeviceSlowDown {
				interval += 5 * time.Second
			}
			if err := wait(ctx, interval, expiresAt); err != nil {
				return TokenResponse{}, err
			}
		default:
			return TokenResponse{}, errors.New("unknown device authorization status")
		}
	}
}

func wait(ctx context.Context, interval time.Duration, expiresAt time.Time) error {
	if !expiresAt.IsZero() {
		remaining := time.Until(expiresAt)
		if remaining <= 0 {
			return ErrDeviceExpired
		}
		if interval > remaining {
			interval = remaining
		}
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrDeviceTimeout
		}
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
