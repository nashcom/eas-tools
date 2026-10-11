package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/nashcom/eas-tools/client"
)

// contactsCommand runs "contacts list", "contacts get", "contacts vcf" and "contacts export".
func contactsCommand(ctx context.Context, m *client.Manager, account string, args []string, stderr io.Writer) (any, error) {
	if len(args) == 0 {
		return nil, usageError("contacts needs list, get, vcf or export")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("contacts "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch sub {
	case "list", "export":
		search := fs.String("search", "", "only the contacts with this text in the name, an e-mail address, the company or a phone number")
		limit := fs.String("limit", "50", "list: number of contacts, or all")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 1 {
			return nil, usageError("contacts " + sub + " takes at most one folder")
		}
		folder := ""
		if len(pos) == 1 {
			folder = pos[0]
		}
		if sub == "export" {
			return m.Export(ctx, account, folder, *search)
		}
		p := client.ContactParams{Search: *search}
		if *limit == "all" {
			p.Limit = -1
		} else if _, err := fmt.Sscanf(*limit, "%d", &p.Limit); err != nil || p.Limit < 1 {
			return nil, usageError("-limit is a number of 1 or more, or all")
		}
		return m.Contacts(ctx, account, folder, p)

	case "get", "vcf":
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "contacts "+sub)
		if err != nil {
			return nil, err
		}
		if sub == "vcf" {
			return m.VCard(ctx, account, folder, id)
		}
		return m.Contact(ctx, account, folder, id)
	}
	return nil, usageError(fmt.Sprintf("contacts %q: use list, get, vcf or export", sub))
}
