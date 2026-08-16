// Package push implements mobile push notifications for LEVEE.
// It supports Apple Push Notification Service (APNs) and Firebase Cloud
// Messaging (FCM) for delivering approval requests and change notifications
// to mobile devices.
//
// The package is organised around four main types:
//
//   - APNSClient          — Apple Push Notification Service client (HTTP/2 + JWT).
//   - FCMClient           — Firebase Cloud Messaging client (HTTP v1 + OAuth2).
//   - PushManager         — unified manager that routes messages to APNs or FCM
//     based on the registered device platform.
//   - DeepLinkGenerator   — generates and validates one-tap approval deep links.
//
// All clients use only the Go standard library (net/http, crypto/*, encoding/*)
// and do not depend on any external SDK. They are safe for concurrent use.
package push

import "errors"

// Sentinel errors returned by the push package. Callers may use errors.Is to
// match on them regardless of the wrapped message.
var (
	// ErrDeviceNotFound is returned when an operation targets a device token
	// that has not been registered for the given user.
	ErrDeviceNotFound = errors.New("push: device not found")

	// ErrPushFailed is returned when the upstream push service (APNs or FCM)
	// rejects the message or returns a non-2xx status.
	ErrPushFailed = errors.New("push: send failed")

	// ErrInvalidToken is returned when a deep-link token is malformed, unknown
	// or has already been consumed.
	ErrInvalidToken = errors.New("push: invalid token")

	// ErrTokenExpired is returned when a deep-link token exists but its TTL
	// has elapsed.
	ErrTokenExpired = errors.New("push: token expired")

	// ErrUnknownPlatform is returned when a device platform is neither "ios"
	// nor "android".
	ErrUnknownPlatform = errors.New("push: unknown platform")

	// ErrEmptyDeviceToken is returned when a device token is the empty string.
	ErrEmptyDeviceToken = errors.New("push: empty device token")

	// ErrEmptyUserID is returned when a user id is the empty string.
	ErrEmptyUserID = errors.New("push: empty user id")
)
