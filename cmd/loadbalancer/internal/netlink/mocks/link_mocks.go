// /*
// Copyright ApeCloud, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */
//
//

// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/vishvananda/netlink (interfaces: Link)

// Package mock_netlink is a generated GoMock package.
package mock_netlink

import (
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
	netlink "github.com/vishvananda/netlink"
)

// MockLink is a mock of Link interface.
type MockLink struct {
	ctrl     *gomock.Controller
	recorder *MockLinkMockRecorder
}

// MockLinkMockRecorder is the mock recorder for MockLink.
type MockLinkMockRecorder struct {
	mock *MockLink
}

// NewMockLink creates a new mock instance.
func NewMockLink(ctrl *gomock.Controller) *MockLink {
	mock := &MockLink{ctrl: ctrl}
	mock.recorder = &MockLinkMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockLink) EXPECT() *MockLinkMockRecorder {
	return m.recorder
}

// Attrs mocks base method.
func (m *MockLink) Attrs() *netlink.LinkAttrs {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Attrs")
	ret0, _ := ret[0].(*netlink.LinkAttrs)
	return ret0
}

// Attrs indicates an expected call of Attrs.
func (mr *MockLinkMockRecorder) Attrs() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Attrs", reflect.TypeOf((*MockLink)(nil).Attrs))
}

// Type mocks base method.
func (m *MockLink) Type() string {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Type")
	ret0, _ := ret[0].(string)
	return ret0
}

// Type indicates an expected call of Type.
func (mr *MockLinkMockRecorder) Type() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Type", reflect.TypeOf((*MockLink)(nil).Type))
}