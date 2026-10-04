// Package all is the single place in the program that knows the complete list
// of client modules.  Adding or removing a client is a one-line change here
// plus the module's own directory; no existing module is ever touched.
package all

import (
	// Blank imports run each module's init(), which calls core.Register.
	_ "client2api/clients/cline"
	_ "client2api/clients/codearts"
	_ "client2api/clients/kimi"
	_ "client2api/clients/lobsterai"
	_ "client2api/clients/loomy"
	_ "client2api/clients/minimaxcode"
	_ "client2api/clients/opencode"
	_ "client2api/clients/openrouter"
	_ "client2api/clients/qwenwork"
	_ "client2api/clients/raccoon"
	_ "client2api/clients/tabbit"
	_ "client2api/clients/trae"
	_ "client2api/clients/workbuddy"
	_ "client2api/clients/zcode"
)
