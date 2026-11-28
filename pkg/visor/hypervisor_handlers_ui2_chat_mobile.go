//go:build mobile

// Package visor pkg/visor/hypervisor_handlers_ui2_chat_mobile.go c3-vis-core
//
// Mobile build: the phone ships no manager UI (static_mobile.go), so the ui2
// chat tab and its handlers stay out of the binary; the chat tab answers as
// a tab this build does not have.
package visor

import "github.com/skycoin/skywire/pkg/httputil"

func (hv *Hypervisor) ui2ChatRoutes(_ *httputil.Router) {}

type ui2ChatState struct{}
