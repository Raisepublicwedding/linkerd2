package destination

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/runconduit/conduit/controller/k8s"
	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
)

var dnsCharactersRegexp = regexp.MustCompile("^[a-zA-Z0-9_-]{0,63}$")
var containsAlphaRegexp = regexp.MustCompile("[a-zA-Z]")

type k8sResolver struct {
	k8sDNSZoneLabels []string
	endpointsWatcher k8s.EndpointsWatcher
	dnsWatcher       DnsWatcher
}

func (k *k8sResolver) canResolve(host string, port int) (bool, error) {
	name, err := localKubernetesServiceIdFromDNSName(k.k8sDNSZoneLabels, host)
	if err != nil {
		return false, err
	}

	return name != nil, nil
}

func (k *k8sResolver) streamResolution(host string, port int, listener updateListener) error {
	serviceName, err := localKubernetesServiceIdFromDNSName(k.k8sDNSZoneLabels, host)
	if err != nil {
		log.Error(err)
		return err
	}

	if serviceName == nil {
		// TODO: Resolve name using DNS similar to Kubernetes' ClusterFirst
		// resolution.
		err = fmt.Errorf("cannot resolve service that isn't a local Kubernetes service: %s", host)
		log.Error(err)
		return err
	}

	svc, exists, err := k.endpointsWatcher.GetService(*serviceName)
	if err != nil {
		log.Errorf("error retrieving service [%s]: %s", *serviceName, err)
		return err
	}

	fmt.Println("BAN ANA", svc.Labels, svc.Annotations)
	if exists && svc.Spec.Type == v1.ServiceTypeExternalName {
		return k.resolveExternalName(svc.Spec.ExternalName, listener)
	}

	return k.resolveKubernetesService(*serviceName, port, listener)
}

func (s *k8sResolver) resolveKubernetesService(id string, port int, listener updateListener) error {
	s.endpointsWatcher.Subscribe(id, uint32(port), listener)

	<-listener.Done()

	s.endpointsWatcher.Unsubscribe(id, uint32(port), listener)

	return nil
}

func (s *k8sResolver) resolveExternalName(externalName string, listener updateListener) error {
	s.dnsWatcher.Subscribe(externalName, listener)

	<-listener.Done()

	s.dnsWatcher.Unsubscribe(externalName, listener)

	return nil
}

// localKubernetesServiceIdFromDNSName returns the name of the service in
// "namespace-name/service-name" form if `host` is a DNS name in a form used
// for local Kubernetes services. It returns nil if `host` isn't in such a
// form.
func localKubernetesServiceIdFromDNSName(k8sDNSZoneLabels []string, host string) (*string, error) {
	hostLabels, err := splitDNSName(host)
	if err != nil {
		return nil, err
	}

	// Verify that `host` ends with ".svc.$zone", ".svc.cluster.local," or ".svc".
	matched := false
	if len(k8sDNSZoneLabels) > 0 {
		hostLabels, matched = maybeStripSuffixLabels(hostLabels, k8sDNSZoneLabels)
	}
	// Accept "cluster.local" as an alias for "$zone". The Kubernetes DNS
	// specification
	// (https://github.com/kubernetes/dns/blob/master/docs/specification.md)
	// doesn't require Kubernetes to do this, but some hosting providers like
	// GKE do it, and so we need to support it for transparency.
	if !matched {
		hostLabels, matched = maybeStripSuffixLabels(hostLabels, []string{"cluster", "local"})
	}
	// TODO:
	// ```
	// 	if !matched {
	//		return nil, nil
	//  }
	// ```
	//
	// This is technically wrong since the protocol definition for the
	// Destination service indicates that `host` is a FQDN and so we should
	// never append a ".$zone" suffix to it, but we need to do this as a
	// workaround until the proxies are configured to know "$zone."
	hostLabels, matched = maybeStripSuffixLabels(hostLabels, []string{"svc"})
	if !matched {
		return nil, nil
	}

	// Extract the service name and namespace. TODO: Federated services have
	// *three* components before "svc"; see
	// https://github.com/runconduit/conduit/issues/156.
	if len(hostLabels) != 2 {
		return nil, fmt.Errorf("not a service: %s", host)
	}
	service := hostLabels[0]
	namespace := hostLabels[1]

	id := namespace + "/" + service
	return &id, nil
}

func splitDNSName(dnsName string) ([]string, error) {
	// If the name is fully qualified, strip off the final dot.
	if strings.HasSuffix(dnsName, ".") {
		dnsName = dnsName[:len(dnsName)-1]
	}

	labels := strings.Split(dnsName, ".")

	// Rejects any empty labels, which is especially important to do for
	// the beginning and the end because we do matching based on labels'
	// relative positions. For example, we need to reject ".example.com"
	// instead of splitting it into ["", "example", "com"].
	for _, l := range labels {
		if l == "" {
			return []string{}, errors.New("Empty label in DNS name: " + dnsName)
		}
		if !dnsCharactersRegexp.MatchString(l) {
			return []string{}, errors.New("DNS name is too long or contains invalid characters: " + dnsName)
		}
		if strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return []string{}, errors.New("DNS name cannot start or end with a dash: " + dnsName)
		}
		if !containsAlphaRegexp.MatchString(l) {
			return []string{}, errors.New("DNS name cannot only contain digits and hyphens: " + dnsName)
		}
	}
	return labels, nil
}

func maybeStripSuffixLabels(input []string, suffix []string) ([]string, bool) {
	n := len(input) - len(suffix)
	if n < 0 {
		return input, false
	}
	if !reflect.DeepEqual(input[n:], suffix) {
		return input, false
	}
	return input[:n], true
}