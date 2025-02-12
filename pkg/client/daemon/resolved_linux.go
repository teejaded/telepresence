package daemon

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/datawire/dlib/dtime"

	"github.com/datawire/dlib/dcontext"
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/client/daemon/dbus"
	"github.com/telepresenceio/telepresence/v2/pkg/client/daemon/dns"
	"github.com/telepresenceio/telepresence/v2/pkg/tun"
)

func (o *outbound) tryResolveD(c context.Context, dev *tun.Device) error {
	// Connect to ResolveD via DBUS.
	if !dbus.IsResolveDRunning(c) {
		dlog.Error(c, "systemd-resolved is not running")
		return errResolveDNotConfigured
	}

	o.setSearchPathFunc = func(c context.Context, paths []string) {
		// When using systemd-resolved, we provide resolution of NAME.NAMESPACE by adding each
		// namespace as a route (a search entry prefixed with ~)
		namespaces := make(map[string]struct{})
		search := make([]string, 0)
		for i, path := range paths {
			if strings.ContainsRune(path, '.') {
				search = append(search, path)
			} else {
				namespaces[path] = struct{}{}
				// Turn namespace into a route
				paths[i] = "~" + path
			}
		}
		for _, sfx := range o.dnsConfig.IncludeSuffixes {
			paths = append(paths, "~"+strings.TrimPrefix(sfx, "."))
		}
		paths = append(paths, kubernetesZone+".")
		namespaces[tel2SubDomain] = struct{}{}

		o.domainsLock.Lock()
		o.namespaces = namespaces
		o.search = search
		o.domainsLock.Unlock()
		if err := dbus.SetLinkDomains(c, int(dev.Index()), paths...); err != nil {
			dlog.Errorf(c, "failed to set link domains on %q: %v", dev.Name(), err)
		} else {
			dlog.Debugf(c, "Link domains on device %q set to [%s]", dev.Name(), strings.Join(paths, ","))
		}
	}

	g := dgroup.NewGroup(c, dgroup.GroupConfig{})

	// DNS resolver
	initDone := make(chan struct{})

	var dnsServer *dns.Server
	g.Go("Server", func(c context.Context) error {
		select {
		case <-c.Done():
			initDone <- struct{}{}
			return nil
		case dnsIP := <-o.kubeDNS:
			listeners, err := o.dnsListeners(c)
			if err != nil {
				dlog.Error(c, err)
				initDone <- struct{}{}
				return err
			}
			// Create a new local address that the DNS resolver can listen to.
			dnsResolverAddr, err := splitToUDPAddr(listeners[0].LocalAddr())
			if err != nil {
				return err
			}

			o.router.configureDNS(c, dnsIP, uint16(53), dnsResolverAddr)
			dlog.Infof(c, "Configuring DNS IP %s", dnsIP)
			if err = dbus.SetLinkDNS(c, int(dev.Index()), dnsIP); err != nil {
				dlog.Error(c, err)
				initDone <- struct{}{}
				return errResolveDNotConfigured
			}
			defer func() {
				// It's very likely that the context is cancelled here. We use it
				// anyway, stripped from cancellation, to retain logging.
				c, cancel := context.WithTimeout(dcontext.WithoutCancel(c), time.Second)
				defer cancel()
				dlog.Debugf(c, "Reverting Link settings for %s", dev.Name())
				o.setSearchPathFunc = nil
				o.router.configureDNS(c, nil, 0, nil) // Don't route from TUN-device
				if err = dbus.RevertLink(c, int(dev.Index())); err != nil {
					dlog.Error(c, err)
				}

				// No need to close listeners here. They are closed by the dnsServer
			}()
			// Some installation have default DNS configured with ~. routing path.
			// If two interfaces with DefaultRoute: yes present, the one with the
			// routing key used and and SanityCheck fails. Hence, tel2SubDomain
			// must be used as a routing key.
			o.domainsLock.Lock()
			namespaces := make(map[string]struct{})
			namespaces[tel2SubDomain] = struct{}{}
			o.namespaces = namespaces
			paths := []string{"~" + tel2SubDomainDot}
			o.search = paths
			o.domainsLock.Unlock()

			if err := dbus.SetLinkDomains(c, int(dev.Index()), paths...); err != nil {
				dlog.Errorf(c, "failed to set link domains on %q: %v", dev.Name(), err)
			} else {
				dlog.Debugf(c, "Link domains on device %q set to [%s]", dev.Name(), strings.Join(paths, ","))
			}
			dnsServer = dns.NewServer(c, listeners, nil, o.resolveInCluster)
			close(initDone)
			return dnsServer.Run(c)
		}
	})
	g.Go("SanityCheck", func(c context.Context) error {
		if _, ok := <-initDone; ok {
			// initDone was not closed, bail out.
			return errResolveDNotConfigured
		}

		// Check if an attempt to resolve a DNS address reaches our DNS resolver, Two seconds should be plenty
		cmdC, cmdCancel := context.WithTimeout(c, 2*time.Second)
		defer cmdCancel()
		for cmdC.Err() == nil {
			dtime.SleepWithContext(cmdC, 100*time.Millisecond)
			_, _ = net.DefaultResolver.LookupHost(cmdC, "jhfweoitnkgyeta."+tel2SubDomain)
			if dnsServer.RequestCount() > 0 {
				close(o.dnsConfigured)
				return nil
			}
			dns.Flush(c)
		}
		dlog.Error(c, "resolver did not receive requests from systemd-resolved")
		return errResolveDNotConfigured
	})
	return g.Wait()
}
