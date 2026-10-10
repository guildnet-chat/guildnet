// SPDX-License-Identifier: AGPL-3.0-only

// Command peek shows what a desk session is doing (a debugging aid).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

func main() {
	c := anthropic.NewClient()
	ctx := context.Background()
	s, err := c.Beta.Sessions.Get(ctx, os.Args[1], anthropic.BetaSessionGetParams{})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("status %s, cost %s %s, updated %s\n", s.Status, s.Usage.ListCost.Amount, s.Usage.ListCost.Currency, s.UpdatedAt)
	iter := c.Beta.Sessions.Events.ListAutoPaging(ctx, os.Args[1], anthropic.BetaSessionEventListParams{})
	for iter.Next() {
		e := iter.Current()
		raw := e.RawJSON()
		if len(os.Args) < 3 && len(raw) > 300 {
			raw = raw[:300] + "…"
		}
		fmt.Println(e.Type, strings.ReplaceAll(raw, "\n", " "))
	}
	if err := iter.Err(); err != nil {
		fmt.Println(err)
	}
}
