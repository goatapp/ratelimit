// Code generated by MockGen. DO NOT EDIT.
// Source: /Users/renuka/git/ratelimit/src/provider/provider.go

// Package mock_provider is a generated GoMock package.
package mock_provider

import (
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"

	config "github.com/goatapp/ratelimit/src/config"
	provider "github.com/goatapp/ratelimit/src/provider"
)

// MockRateLimitConfigProvider is a mock of RateLimitConfigProvider interface
type MockRateLimitConfigProvider struct {
	ctrl     *gomock.Controller
	recorder *MockRateLimitConfigProviderMockRecorder
}

// MockRateLimitConfigProviderMockRecorder is the mock recorder for MockRateLimitConfigProvider
type MockRateLimitConfigProviderMockRecorder struct {
	mock *MockRateLimitConfigProvider
}

// NewMockRateLimitConfigProvider creates a new mock instance
func NewMockRateLimitConfigProvider(ctrl *gomock.Controller) *MockRateLimitConfigProvider {
	mock := &MockRateLimitConfigProvider{ctrl: ctrl}
	mock.recorder = &MockRateLimitConfigProviderMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockRateLimitConfigProvider) EXPECT() *MockRateLimitConfigProviderMockRecorder {
	return m.recorder
}

// ConfigUpdateEvent mocks base method
func (m *MockRateLimitConfigProvider) ConfigUpdateEvent() <-chan provider.ConfigUpdateEvent {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ConfigUpdateEvent")
	ret0, _ := ret[0].(<-chan provider.ConfigUpdateEvent)
	return ret0
}

// ConfigUpdateEvent indicates an expected call of ConfigUpdateEvent
func (mr *MockRateLimitConfigProviderMockRecorder) ConfigUpdateEvent() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ConfigUpdateEvent", reflect.TypeOf((*MockRateLimitConfigProvider)(nil).ConfigUpdateEvent))
}

// Stop mocks base method
func (m *MockRateLimitConfigProvider) Stop() {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "Stop")
}

// Stop indicates an expected call of Stop
func (mr *MockRateLimitConfigProviderMockRecorder) Stop() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Stop", reflect.TypeOf((*MockRateLimitConfigProvider)(nil).Stop))
}

// MockConfigUpdateEvent is a mock of ConfigUpdateEvent interface
type MockConfigUpdateEvent struct {
	ctrl     *gomock.Controller
	recorder *MockConfigUpdateEventMockRecorder
}

// MockConfigUpdateEventMockRecorder is the mock recorder for MockConfigUpdateEvent
type MockConfigUpdateEventMockRecorder struct {
	mock *MockConfigUpdateEvent
}

// NewMockConfigUpdateEvent creates a new mock instance
func NewMockConfigUpdateEvent(ctrl *gomock.Controller) *MockConfigUpdateEvent {
	mock := &MockConfigUpdateEvent{ctrl: ctrl}
	mock.recorder = &MockConfigUpdateEventMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockConfigUpdateEvent) EXPECT() *MockConfigUpdateEventMockRecorder {
	return m.recorder
}

// GetConfig mocks base method
func (m *MockConfigUpdateEvent) GetConfig() (config.RateLimitConfig, any) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetConfig")
	ret0, _ := ret[0].(config.RateLimitConfig)
	ret1, _ := ret[1].(any)
	return ret0, ret1
}

// GetConfig indicates an expected call of GetConfig
func (mr *MockConfigUpdateEventMockRecorder) GetConfig() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetConfig", reflect.TypeOf((*MockConfigUpdateEvent)(nil).GetConfig))
}
