package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/AbsaOSS/k8gb/controllers/internal/utils"
	"strings"
	"time"

	"github.com/AbsaOSS/k8gb/controllers/providers"
	"github.com/AbsaOSS/k8gb/controllers/providers/route53"

	"github.com/AbsaOSS/k8gb/controllers/providers/ns1"

	"github.com/AbsaOSS/k8gb/controllers/providers/infoblox"

	"github.com/AbsaOSS/k8gb/controllers/depresolver"

	coreerrors "errors"

	k8gbv1beta1 "github.com/AbsaOSS/k8gb/api/v1beta1"
	ibclient "github.com/infobloxopen/infoblox-go-client"
	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldns "sigs.k8s.io/external-dns/endpoint"
)

const coreDNSExtServiceName = "k8gb-coredns-lb"

func (r *GslbReconciler) getGslbIngressIPs(gslb *k8gbv1beta1.Gslb) ([]string, error) {
	nn := types.NamespacedName{
		Name:      gslb.Name,
		Namespace: gslb.Namespace,
	}

	gslbIngress := &v1beta1.Ingress{}

	err := r.Get(context.TODO(), nn, gslbIngress)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info(fmt.Sprintf("Can't find gslb Ingress: %s", gslb.Name))
		}
		return nil, err
	}

	var gslbIngressIPs []string

	for _, ip := range gslbIngress.Status.LoadBalancer.Ingress {
		if len(ip.IP) > 0 {
			gslbIngressIPs = append(gslbIngressIPs, ip.IP)
		}
		if len(ip.Hostname) > 0 {
			IPs, err := utils.Dig(r.Config.EdgeDNSServer, ip.Hostname)
			if err != nil {
				log.Info("can't dig %s; %s",r.Config.EdgeDNSServer, err.Error())
				return nil, err
			}
			gslbIngressIPs = append(gslbIngressIPs, IPs...)
		}
	}

	return gslbIngressIPs, nil
}

func getExternalClusterHeartbeatFQDNs(gslb *k8gbv1beta1.Gslb, config *depresolver.Config) (extGslbClusters []string) {
	for _, geoTag := range config.ExtClustersGeoTags {
		extGslbClusters = append(extGslbClusters, fmt.Sprintf("%s-heartbeat-%s.%s", gslb.Name, geoTag, config.EdgeDNSZone))
	}
	return
}

func (r *GslbReconciler) getExternalTargets(host string) ([]string, error) {

	extGslbClusters := r.nsServerNameExt()

	var targets []string

	for _, cluster := range extGslbClusters {
		log.Info(fmt.Sprintf("Adding external Gslb targets from %s cluster...", cluster))
		g := new(dns.Msg)
		host = fmt.Sprintf("localtargets-%s.", host) // Convert to true FQDN with dot at the end. Otherwise dns lib freaks out
		g.SetQuestion(host, dns.TypeA)

		ns := overrideWithFakeDNS(r.Config.Override.FakeDNSEnabled, cluster)

		a, err := dns.Exchange(g, ns)
		if err != nil {
			log.Info(fmt.Sprintf("Error contacting external Gslb cluster(%s) : (%v)", cluster, err))
			return nil, nil
		}
		var clusterTargets []string

		for _, A := range a.Answer {
			IP := strings.Split(A.String(), "\t")[4]
			clusterTargets = append(clusterTargets, IP)
		}
		if len(clusterTargets) > 0 {
			targets = append(targets, clusterTargets...)
			log.Info(fmt.Sprintf("Added external %s Gslb targets from %s cluster", clusterTargets, cluster))
		}
	}

	return targets, nil
}

func (r *GslbReconciler) gslbDNSEndpoint(gslb *k8gbv1beta1.Gslb) (*externaldns.DNSEndpoint, error) {
	var gslbHosts []*externaldns.Endpoint
	var ttl = externaldns.TTL(gslb.Spec.Strategy.DNSTtlSeconds)

	serviceHealth, err := r.getServiceHealthStatus(gslb)
	if err != nil {
		return nil, err
	}

	localTargets, err := r.getGslbIngressIPs(gslb)
	if err != nil {
		return nil, err
	}

	for host, health := range serviceHealth {
		var finalTargets []string

		if !strings.Contains(host, r.Config.EdgeDNSZone) {
			return nil, fmt.Errorf("ingress host %s does not match delegated zone %s", host, r.Config.EdgeDNSZone)
		}

		if health == "Healthy" {
			finalTargets = append(finalTargets, localTargets...)
			localTargetsHost := fmt.Sprintf("localtargets-%s", host)
			dnsRecord := &externaldns.Endpoint{
				DNSName:    localTargetsHost,
				RecordTTL:  ttl,
				RecordType: "A",
				Targets:    localTargets,
			}
			gslbHosts = append(gslbHosts, dnsRecord)
		}

		// Check if host is alive on external Gslb
		externalTargets, err := r.getExternalTargets(host)
		if err != nil {
			return nil, err
		}
		if len(externalTargets) > 0 {
			switch gslb.Spec.Strategy.Type {
			case roundRobinStrategy:
				finalTargets = append(finalTargets, externalTargets...)
			case failoverStrategy:
				// If cluster is Primary
				if gslb.Spec.Strategy.PrimaryGeoTag == r.Config.ClusterGeoTag {
					// If cluster is Primary and Healthy return only own targets
					// If cluster is Primary and Unhealthy return Secondary external targets
					if health != "Healthy" {
						finalTargets = externalTargets
						log.Info(fmt.Sprintf("Executing failover strategy for %s Gslb on Primary. Workload on primary %s cluster is unhealthy, targets are %v",
							gslb.Name, gslb.Spec.Strategy.PrimaryGeoTag, finalTargets))
					}
				} else {
					// If cluster is Secondary and Primary external cluster is Healthy
					// then return Primary external targets.
					// Return own targets by default.
					finalTargets = externalTargets
					log.Info(fmt.Sprintf("Executing failover strategy for %s Gslb on Secondary. Workload on primary %s cluster is healthy, targets are %v",
						gslb.Name, gslb.Spec.Strategy.PrimaryGeoTag, finalTargets))
				}
			}
		} else {
			log.Info(fmt.Sprintf("No external targets have been found for host %s", host))
		}

		log.Info(fmt.Sprintf("Final target list for %s Gslb: %v", gslb.Name, finalTargets))

		if len(finalTargets) > 0 {
			dnsRecord := &externaldns.Endpoint{
				DNSName:    host,
				RecordTTL:  ttl,
				RecordType: "A",
				Targets:    finalTargets,
			}
			gslbHosts = append(gslbHosts, dnsRecord)
		}
	}
	dnsEndpointSpec := externaldns.DNSEndpointSpec{
		Endpoints: gslbHosts,
	}

	dnsEndpoint := &externaldns.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:        gslb.Name,
			Namespace:   gslb.Namespace,
			Annotations: map[string]string{"k8gb.absa.oss/dnstype": "local"},
		},
		Spec: dnsEndpointSpec,
	}

	err = controllerutil.SetControllerReference(gslb, dnsEndpoint, r.Scheme)
	if err != nil {
		return nil, err
	}
	return dnsEndpoint, err
}

func (r *GslbReconciler) nsServerNameExt() []string {

	dnsZoneIntoNS := strings.ReplaceAll(r.Config.DNSZone, ".", "-")
	var extNSServers []string
	for _, clusterGeoTag := range r.Config.ExtClustersGeoTags {
		extNSServers = append(extNSServers,
			fmt.Sprintf("gslb-ns-%s-%s.%s",
				dnsZoneIntoNS,
				clusterGeoTag,
				r.Config.EdgeDNSZone))
	}

	return extNSServers
}

func checkAliveFromTXT(fqdn string, config *depresolver.Config, splitBrainThreshold time.Duration) error {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(fqdn), dns.TypeTXT)
	ns := overrideWithFakeDNS(config.Override.FakeDNSEnabled, config.EdgeDNSServer)
	txt, err := dns.Exchange(m, ns)
	if err != nil {
		log.Info(fmt.Sprintf("Error contacting EdgeDNS server (%s) for TXT split brain record: (%s)", ns, err))
		return err
	}
	var timestamp string
	if len(txt.Answer) > 0 {
		if t, ok := txt.Answer[0].(*dns.TXT); ok {
			log.Info(fmt.Sprintf("Split brain TXT raw record: %s", t.String()))
			timestamp = strings.Split(t.String(), "\t")[4]
			timestamp = strings.Trim(timestamp, "\"") // Otherwise time.Parse() will miserably fail
		}
	}

	if len(timestamp) > 0 {
		log.Info(fmt.Sprintf("Split brain TXT raw time stamp: %s", timestamp))
		timeFromTXT, err := time.Parse("2006-01-02T15:04:05", timestamp)
		if err != nil {
			return err
		}

		log.Info(fmt.Sprintf("Split brain TXT parsed time stamp: %s", timeFromTXT))
		now := time.Now().UTC()

		diff := now.Sub(timeFromTXT)
		log.Info(fmt.Sprintf("Split brain TXT time diff: %s", diff))

		if diff > splitBrainThreshold {
			return errors.NewGone(fmt.Sprintf("Split brain TXT record expired the time threshold: (%s)", splitBrainThreshold))
		}

		return nil
	}
	return errors.NewGone(fmt.Sprintf("Can't find split brain TXT record at EdgeDNS server(%s) and record %s ", ns, fqdn))

}

func filterOutDelegateTo(delegateTo []ibclient.NameServer, fqdn string) []ibclient.NameServer {
	for i := 0; i < len(delegateTo); i++ {
		if delegateTo[i].Name == fqdn {
			delegateTo = append(delegateTo[:i], delegateTo[i+1:]...)
			i--
		}
	}
	return delegateTo
}


func (r *GslbReconciler) coreDNSExposedIPs() ([]string, error) {
	coreDNSService := &corev1.Service{}

	err := r.Get(context.TODO(), types.NamespacedName{Namespace: k8gbNamespace, Name: coreDNSExtServiceName}, coreDNSService)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info(fmt.Sprintf("Can't find %s service", coreDNSExtServiceName))
		}
		return nil, err
	}
	var lbHostname string
	if len(coreDNSService.Status.LoadBalancer.Ingress) > 0 {
		lbHostname = coreDNSService.Status.LoadBalancer.Ingress[0].Hostname
	} else {
		errMessage := fmt.Sprintf("no Ingress LoadBalancer entries found for %s serice", coreDNSExtServiceName)
		log.Info(errMessage)
		err := coreerrors.New(errMessage)
		return nil, err
	}
	IPs, err := utils.Dig(r.Config.EdgeDNSServer, lbHostname)
	if err != nil {
		log.Info(fmt.Sprintf("Can't dig k8gb-coredns-lb service loadbalancer fqdn %s", lbHostname))
		return nil, err
	}
	return IPs, nil
}

func (r *GslbReconciler) configureZoneDelegation(gslb *k8gbv1beta1.Gslb) (*reconcile.Result, error) {
	var provider providers.IDnsProvider
	var err error
	switch r.Config.EdgeDNSType {
	case depresolver.DNSTypeRoute53:
		provider, err = route53.NewRoute53(r.Config, gslb, r.Client)
	case depresolver.DNSTypeNS1:
		provider, err = ns1.NewNs1(r.Config, gslb, r.Client)
	case depresolver.DNSTypeInfoblox:
		provider, err = infoblox.NewInfoblox(r.Config, gslb, r.Client)
	case depresolver.DNSTypeNoEdgeDNS:
		return nil, nil
	default:
		return nil, coreerrors.New("unhandled DNS type")
	}
	if err != nil {
		return nil, err
	}
	return provider.ConfigureZoneDelegation()
}

func (r *GslbReconciler) ensureDNSEndpoint(
	namespace string,
	i *externaldns.DNSEndpoint,
) (*reconcile.Result, error) {
	found := &externaldns.DNSEndpoint{}
	err := r.Get(context.TODO(), types.NamespacedName{
		Name:      i.Name,
		Namespace: namespace,
	}, found)
	if err != nil && errors.IsNotFound(err) {

		// Create the DNSEndpoint
		log.Info(fmt.Sprintf("Creating a new DNSEndpoint:\n %s", prettyPrint(i)))
		err = r.Create(context.TODO(), i)

		if err != nil {
			// Creation failed
			log.Error(err, "Failed to create new DNSEndpoint", "DNSEndpoint.Namespace", i.Namespace, "DNSEndpoint.Name", i.Name)
			return &reconcile.Result{}, err
		}
		// Creation was successful
		return nil, nil
	} else if err != nil {
		// Error that isn't due to the service not existing
		log.Error(err, "Failed to get DNSEndpoint")
		return &reconcile.Result{}, err
	}

	// Update existing object with new spec
	found.Spec = i.Spec
	err = r.Update(context.TODO(), found)

	if err != nil {
		// Update failed
		log.Error(err, "Failed to update DNSEndpoint", "DNSEndpoint.Namespace", found.Namespace, "DNSEndpoint.Name", found.Name)
		return &reconcile.Result{}, err
	}

	return nil, nil
}

func overrideWithFakeDNS(fakeDNSEnabled bool, server string) (ns string) {
	if fakeDNSEnabled {
		ns = "127.0.0.1:7753"
	} else {
		ns = fmt.Sprintf("%s:53", server)
	}
	return
}

func prettyPrint(s interface{}) string {
	prettyStruct, err := json.MarshalIndent(s, "", "\t")
	if err != nil {
		fmt.Println("can't convert struct to json")
	}
	return string(prettyStruct)
}
