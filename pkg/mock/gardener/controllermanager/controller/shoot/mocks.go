// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/gardener/gardener/pkg/controllermanager/controller/shoot (interfaces: Cron)

// Package shoot is a generated GoMock package.
package shoot

import (
	gomock "github.com/golang/mock/gomock"
	cron "github.com/robfig/cron"
	reflect "reflect"
)

// MockCron is a mock of Cron interface
type MockCron struct {
	ctrl     *gomock.Controller
	recorder *MockCronMockRecorder
}

// MockCronMockRecorder is the mock recorder for MockCron
type MockCronMockRecorder struct {
	mock *MockCron
}

// NewMockCron creates a new mock instance
func NewMockCron(ctrl *gomock.Controller) *MockCron {
	mock := &MockCron{ctrl: ctrl}
	mock.recorder = &MockCronMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockCron) EXPECT() *MockCronMockRecorder {
	return m.recorder
}

// Schedule mocks base method
func (m *MockCron) Schedule(arg0 cron.Schedule, arg1 cron.Job) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "Schedule", arg0, arg1)
}

// Schedule indicates an expected call of Schedule
func (mr *MockCronMockRecorder) Schedule(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Schedule", reflect.TypeOf((*MockCron)(nil).Schedule), arg0, arg1)
}

// Start mocks base method
func (m *MockCron) Start() {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "Start")
}

// Start indicates an expected call of Start
func (mr *MockCronMockRecorder) Start() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Start", reflect.TypeOf((*MockCron)(nil).Start))
}

// Stop mocks base method
func (m *MockCron) Stop() {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "Stop")
}

// Stop indicates an expected call of Stop
func (mr *MockCronMockRecorder) Stop() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Stop", reflect.TypeOf((*MockCron)(nil).Stop))
}