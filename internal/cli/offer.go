package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/safe"
)

// thelb offer asks the backend running on this node to offer one of its
// applications to the cluster, or to withdraw it.

func (a *app) offerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "offer <command>",
		Short: "Offer an application to the cluster, or withdraw it (on a backend)",
		Long: `Offer one of this backend's applications to the cluster, or withdraw it. Runs
on a backend that is a member of a cluster.`,
		Annotations: map[string]string{annDocs: docsURL + "docs/backends.html"},
	}
	add := &cobra.Command{
		Use:   "add <name> <tcp|udp|sctp>/<port>[,check=<check>]",
		Short: "Offer an application",
		Long: `Offer an application under a name of your choice. The leader checks it first,
then records the offer, unassigned, for a director to assign to a service. The
same name again, with another port or check, replaces the offer.

The backend checks what it offers every 2 seconds: while the check fails, the
offer serves no service.

Checks:
  tcp              A TCP connection (TCP's default)
  http:<path>      An HTTP GET of the path: 2xx or 3xx passes
  none             No check (UDP's and SCTP's default)

See also: thelb admin assign`,
		Example: `  thelb offer add shop tcp/8080,check=http:/healthz
  thelb offer add dns udp/53`,
		Args: nArgs(2, "a name and an offer: offer add <name> <tcp|udp|sctp>/<port>[,check=<check>]"),
	}
	add.RunE = a.act(func(_ *cobra.Command, args []string) error {
		if main, _, _ := strings.Cut(args[1], ","); strings.Contains(main, "=") {
			return usagef("the name comes first, then the offer: thelb offer add <name> <proto>/<port>")
		}
		o, err := parseOffer(args[0] + "=" + args[1])
		if err != nil {
			return usagef("%v", err)
		}
		return a.offer(api.Offer{Op: "add", Offer: o})
	})
	remove := &cobra.Command{
		Use:     "remove <name>",
		Short:   "Withdraw an offer",
		Long:    "Withdraw an offer. The services it is assigned to lose it.",
		Example: "  thelb offer remove shop",
		Args:    nArgs(1, "an offer's name: offer remove <name>"),
	}
	remove.RunE = a.act(func(_ *cobra.Command, args []string) error {
		if !cluster.OfferNameOK(args[0]) {
			return usagef("%q is not an offer's name", args[0])
		}
		return a.offer(api.Offer{Op: "remove", Offer: cluster.Offer{Name: args[0]}})
	})
	return a.group(c, add, remove)
}

// offer asks the backend running here, and prints what it did.
func (a *app) offer(req api.Offer) error {
	resp, err := a.call("backend", localsock.Request{Op: localsock.OpOffer, Offer: &req})
	if errors.Is(err, localsock.ErrNotRunning) {
		return errors.New("no backend runs on this node: thelb offer runs where thelb backend runs")
	}
	if err != nil {
		return err
	}
	if r := resp.Result; r != nil && r.Message != "" {
		_, _ = fmt.Fprintln(a.stdout, safe.Text(r.Message))
	}
	return nil
}
