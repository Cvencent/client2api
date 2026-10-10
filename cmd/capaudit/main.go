// Command capaudit prints the real capability matrix for every compiled-in
// module, computed from type assertions the same way the panel does it.
//
// It is a development aid: run it after changing a module's optional
// interfaces to see at a glance what the panel will and will not offer.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	_ "client2api/clients/all"
	"client2api/internal/core"
)

func main() {
	ctx := context.Background()
	names := core.Registered()
	sort.Strings(names)

	type row struct {
		name string
		caps core.Capabilities
	}
	rows := make([]row, 0, len(names))
	for _, name := range names {
		c, err := core.Build(name, core.Deps{})
		if err != nil {
			continue
		}
		rows = append(rows, row{name: name, caps: core.CapabilitiesOf(ctx, c)})
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "client\tcaps\tcapabilities")
	for _, r := range rows {
		var on []string
		for _, p := range pairs(r.caps) {
			if p.on {
				on = append(on, p.name)
			}
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", r.name, len(on), strings.Join(on, ","))
	}
	_ = w.Flush()
}

type pair struct {
	name string
	on   bool
}

func pairs(c core.Capabilities) []pair {
	return []pair{
		{"manage", c.Manage},
		{"import", c.Import},
		{"bundle", c.Bundle},
		{"login", c.Login},
		{"direct_key", c.DirectKey != ""},
		{"key_pages", len(c.KeyPages) > 0},
		{"quick_connect", len(c.QuickConnect) > 0},
		{"realms", len(c.Realms) > 0},
		{"checkin", c.Checkin},
		{"refresh", c.Refresh},
		{"tasks", c.Tasks},
		{"task_accept", c.TaskAccept},
		{"task_claim", c.TaskClaim},
		{"task_auto", c.TaskAuto},
		{"degrade", c.Degrade},
		{"batches", c.Batches},
		{"health", c.Health},
		{"live", c.Live},
		{"revive", c.Revive},
		{"balance", c.Balance},
		{"packages", c.Packages},
		{"vouchers", c.Vouchers},
		{"conversations", c.Conversations},
		{"captcha", c.Captcha},
		{"sms", c.SMS},
		{"auto_login", c.AutoLogin},
	}
}
