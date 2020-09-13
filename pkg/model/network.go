/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package model

import (
	"fmt"
	"strings"

	"github.com/golang/glog"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awstasks"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/aws"
)

// NetworkModelBuilder configures network objects
type NetworkModelBuilder struct {
	*KopsModelContext
	Lifecycle *fi.Lifecycle
}

var _ fi.ModelBuilder = &NetworkModelBuilder{}

type zoneInfo struct {
	PrivateSubnets []*kops.ClusterSubnetSpec
}

func (b *NetworkModelBuilder) Build(c *fi.ModelBuilderContext) error {
	sharedVPC := b.Cluster.SharedVPC()
	vpcName := b.ClusterName()
	tags := b.CloudTags(vpcName, sharedVPC)

	// VPC that holds everything for the cluster
	{
		vpcTags := tags
		if sharedVPC {
			// We don't tag a shared VPC - we can identify it by its ID anyway.  Issue #4265
			vpcTags = nil
		}
		t := &awstasks.VPC{
			Name:             s(vpcName),
			Lifecycle:        b.Lifecycle,
			Shared:           fi.Bool(sharedVPC),
			EnableDNSSupport: fi.Bool(true),
			Tags:             vpcTags,
		}

		if sharedVPC && b.IsKubernetesGTE("1.5") {
			// If we're running k8s 1.5, and we have e.g.  --kubelet-preferred-address-types=InternalIP,Hostname,ExternalIP,LegacyHostIP
			// then we don't need EnableDNSHostnames any more
			glog.V(4).Infof("Kubernetes version %q; skipping EnableDNSHostnames requirement on VPC", b.KubernetesVersion())
		} else {
			// In theory we don't need to enable it for >= 1.5,
			// but seems safer to stick with existing behaviour

			t.EnableDNSHostnames = fi.Bool(true)
		}

		if b.Cluster.Spec.NetworkID != "" {
			t.ID = s(b.Cluster.Spec.NetworkID)
		}

		if b.Cluster.Spec.NetworkCIDR != "" {
			t.CIDR = s(b.Cluster.Spec.NetworkCIDR)
		}

		c.AddTask(t)
	}

	if !sharedVPC {
		for _, cidr := range b.Cluster.Spec.AdditionalNetworkCIDRs {
			c.AddTask(&awstasks.VPCCIDRBlock{
				Name:      s(cidr),
				Lifecycle: b.Lifecycle,
				VPC:       b.LinkToVPC(),
				Shared:    fi.Bool(sharedVPC),
				CIDRBlock: &cidr,
			})
		}
	}

	if !sharedVPC {
		dhcp := &awstasks.DHCPOptions{
			Name:              s(b.ClusterName()),
			Lifecycle:         b.Lifecycle,
			DomainNameServers: s("AmazonProvidedDNS"),

			Tags:   tags,
			Shared: fi.Bool(sharedVPC),
		}
		if b.Region == "us-east-1" {
			dhcp.DomainName = s("ec2.internal")
		} else {
			dhcp.DomainName = s(b.Region + ".compute.internal")
		}
		c.AddTask(dhcp)

		c.AddTask(&awstasks.VPCDHCPOptionsAssociation{
			Name:        s(b.ClusterName()),
			Lifecycle:   b.Lifecycle,
			VPC:         b.LinkToVPC(),
			DHCPOptions: dhcp,
		})
	} else {
		// TODO: would be good to create these as shared, to verify them
	}

	allSubnetsShared := true
	allSubnetsSharedInZone := make(map[string]bool)
	for i := range b.Cluster.Spec.Subnets {
		subnetSpec := &b.Cluster.Spec.Subnets[i]
		allSubnetsSharedInZone[subnetSpec.Zone] = true
	}

	for i := range b.Cluster.Spec.Subnets {
		subnetSpec := &b.Cluster.Spec.Subnets[i]
		sharedSubnet := subnetSpec.ProviderID != ""
		if !sharedSubnet {
			allSubnetsShared = false
			allSubnetsSharedInZone[subnetSpec.Zone] = false
		}
	}

	// We always have a public route table, though for private networks it is only used for NGWs and ELBs
	var publicRouteTable *awstasks.RouteTable
	{
		// The internet gateway is the main entry point to the cluster.
		igw := &awstasks.InternetGateway{
			Name:      s(b.ClusterName()),
			Lifecycle: b.Lifecycle,
			VPC:       b.LinkToVPC(),
			Shared:    fi.Bool(sharedVPC),
		}
		igw.Tags = b.CloudTags(*igw.Name, *igw.Shared)
		c.AddTask(igw)

		if !allSubnetsShared {
			// The route table is not shared if we're creating a subnet for our cluster
			// That subnet will be owned, and will be associated with our RouteTable.
			// On deletion we delete the subnet & the route table.
			sharedRouteTable := false
			routeTableTags := b.CloudTags(vpcName, sharedRouteTable)
			routeTableTags[awsup.TagNameKopsRole] = "public"
			publicRouteTable = &awstasks.RouteTable{
				Name:      s(b.ClusterName()),
				Lifecycle: b.Lifecycle,

				VPC: b.LinkToVPC(),

				Tags:   routeTableTags,
				Shared: fi.Bool(sharedRouteTable),
			}
			c.AddTask(publicRouteTable)

			// TODO: Validate when allSubnetsShared
			c.AddTask(&awstasks.Route{
				Name:            s("0.0.0.0/0"),
				Lifecycle:       b.Lifecycle,
				CIDR:            s("0.0.0.0/0"),
				RouteTable:      publicRouteTable,
				InternetGateway: igw,
			})
		}
	}

	infoByZone := make(map[string]*zoneInfo)

	for i := range b.Cluster.Spec.Subnets {
		subnetSpec := &b.Cluster.Spec.Subnets[i]
		sharedSubnet := subnetSpec.ProviderID != ""
		subnetName := subnetSpec.Name + "." + b.ClusterName()
		tags := map[string]string{}

		// Apply tags so that Kubernetes knows which subnets should be used for internal/external ELBs
		if b.Cluster.Spec.DisableSubnetTags {
			glog.V(2).Infof("skipping subnet tags. Ensure these are maintained externally.")
		} else {
			glog.V(2).Infof("applying subnet tags")
			tags = b.CloudTags(subnetName, sharedSubnet)
			tags["SubnetType"] = string(subnetSpec.Type)
			tags[aws.TagNameSubnetPublicELB] = "1"
		}

		subnet := &awstasks.Subnet{
			Name:             s(subnetName),
			ShortName:        s(subnetSpec.Name),
			Lifecycle:        b.Lifecycle,
			VPC:              b.LinkToVPC(),
			AvailabilityZone: s(subnetSpec.Zone),
			CIDR:             s(subnetSpec.CIDR),
			Shared:           fi.Bool(sharedSubnet),
			Tags:             tags,
		}

		if subnetSpec.ProviderID != "" {
			subnet.ID = s(subnetSpec.ProviderID)
		}
		c.AddTask(subnet)

		if !sharedSubnet {
			c.AddTask(&awstasks.RouteTableAssociation{
				Name:       s(subnetSpec.Name + "." + b.ClusterName()),
				Lifecycle:  b.Lifecycle,
				RouteTable: publicRouteTable,
				Subnet:     subnet,
			})
		}
	}

	// Set up private route tables & egress
	for zone, info := range infoByZone {
		if len(info.PrivateSubnets) == 0 {
			continue
		}

		egress := info.PrivateSubnets[0].Egress
		publicIP := info.PrivateSubnets[0].PublicIP

		// Verify we don't have mixed values for egress/publicIP - the code doesn't handle it
		for _, subnet := range info.PrivateSubnets {
			if subnet.Egress != egress {
				return fmt.Errorf("cannot mix egress values in private subnets")
			}
			if subnet.PublicIP != publicIP {
				return fmt.Errorf("cannot mix publicIP values in private subnets")
			}
		}

		var in *awstasks.Instance
		if egress != "" {
			if strings.HasPrefix(egress, "i-") {

				in = &awstasks.Instance{
					Name:      s(egress),
					Lifecycle: b.Lifecycle,
					ID:        s(egress),
					Shared:    fi.Bool(true),
					Tags:      nil, // We don't need to add tags here
				}

				c.AddTask(in)

			} else {
				return fmt.Errorf("kops currently only supports re-use of either NAT EC2 Instances or NAT Gateways. We will support more eventually! Please see https://github.com/kubernetes/kops/issues/1530")
			}

		}

		// Private Route Table
		//
		// The private route table that will route to the NAT Gateway
		// We create an owned route table if we created any subnet in that zone.
		// Otherwise we consider it shared.
		routeTableShared := allSubnetsSharedInZone[zone]
		routeTableTags := b.CloudTags(b.NamePrivateRouteTableInZone(zone), routeTableShared)
		routeTableTags[awsup.TagNameKopsRole] = "private-" + zone
		rt := &awstasks.RouteTable{
			Name:      s(b.NamePrivateRouteTableInZone(zone)),
			VPC:       b.LinkToVPC(),
			Lifecycle: b.Lifecycle,

			Shared: fi.Bool(routeTableShared),
			Tags:   routeTableTags,
		}
		c.AddTask(rt)

		// Private Routes
		//
		// Routes for the private route table.
		// Will route to the NAT Gateway
		var r *awstasks.Route
		if in != nil {

			r = &awstasks.Route{
				Name:       s("private-" + zone + "-0.0.0.0/0"),
				Lifecycle:  b.Lifecycle,
				CIDR:       s("0.0.0.0/0"),
				RouteTable: rt,
				Instance:   in,
			}

		}
		c.AddTask(r)

	}

	return nil
}
