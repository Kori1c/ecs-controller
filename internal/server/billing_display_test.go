package server

import (
	"testing"

	"github.com/Kori1c/ecs-controller/internal/cloud"
)

func TestBillingResourceEnrichmentAcceptsChargingAttributesWithoutChangingBills(t *testing.T) {
	resources := map[string]cloud.BillingResource{
		"eip-demo": {InstanceID: "i-demo", EIP: &cloud.BillingEIP{AllocationID: "eip-demo", Count: 1, Bandwidth: 200}},
		"i-demo":   {InstanceID: "i-demo", SystemDisk: &cloud.BillingSystemDisk{Size: 2, Category: "cloud_auto"}},
	}
	for _, test := range []struct {
		id   string
		want bool
	}{
		{"eip-demo", true},
		{"eip-demo;cn-hongkong;BGP;0", true},
		{" eip-demo ;cn-hongkong;BGP;0", true},
		{"i-demo;cn-hongkong", true},
		{"eip-other;cn-guangzhou;BGP;0", false},
		{"cn-shenzhen", false},
	} {
		t.Run(test.id, func(t *testing.T) {
			items := []cloud.BillingDetail{{InstanceID: test.id, Amount: 0.00462, Usage: 6, Unit: "Piece", BillingItem: "Public IP Retention Fee"}}
			enrichBillingDetails(items, resources)
			if (items[0].CurrentResource != nil) != test.want {
				t.Fatalf("resource association for %q: %+v", test.id, items[0].CurrentResource)
			}
			if items[0].InstanceID != test.id || items[0].Amount != 0.00462 || items[0].Usage != 6 || items[0].Unit != "Piece" || items[0].BillingItem != "Public IP Retention Fee" {
				t.Fatal("original bill fields were changed")
			}
		})
	}
}

func TestBillingResourceEnrichmentPrefersExactProviderIDs(t *testing.T) {
	id := "eip-demo;cn-hongkong;BGP;0"
	items := []cloud.BillingDetail{{InstanceID: id}}
	enrichBillingDetails(items, map[string]cloud.BillingResource{
		"eip-demo": {EIP: &cloud.BillingEIP{Count: 1}},
		id:         {EIP: &cloud.BillingEIP{Count: 2}},
	})
	if items[0].CurrentResource == nil || items[0].CurrentResource.EIP.Count != 2 {
		t.Fatal("exact resource ID was overridden by a partial match")
	}
}
