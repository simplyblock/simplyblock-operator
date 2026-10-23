// The site link on the CSI controller (design-stretched-sync-replication-ramen.md
// §7.3): on a member whose operator runs the site link, the controller plugin
// links to the site's fleet agent too, with a token for the site link's audience.
// The node plugin never does: a node does not speak across sites.

package driver

import "testing"

func TestTheControllerJoinsTheSiteLinkWhenTheOperatorRunsOne(t *testing.T) {
	cfg := &SiteLinkConfig{Address: "agent.example:9443", AgentNamespace: "agent-ns"}
	sts := withSiteLink(controllerStatefulSet(testDriver("d"), "image"), cfg)
	pod := sts.Spec.Template.Spec
	var plugin = &pod.Containers[0]
	for i := range pod.Containers {
		if pod.Containers[i].Name == controllerPluginName {
			plugin = &pod.Containers[i]
		}
	}
	for _, arg := range []string{"--site-link", "--site-link-address=agent.example:9443", "--site-link-namespace=agent-ns"} {
		if !hasArg(plugin, arg) {
			t.Errorf("the controller plugin lacks %s: %v", arg, plugin.Args)
		}
	}
	v := volumeNamed(pod.Volumes, siteLinkTokenVolumeName)
	if v == nil || v.Projected == nil || v.Projected.Sources[0].ServiceAccountToken.Audience != siteLinkAudience {
		t.Errorf("no projected token with audience %s: %+v", siteLinkAudience, v)
	}
	if mountNamed(plugin.VolumeMounts, siteLinkTokenVolumeName) == nil {
		t.Error("the controller plugin does not mount the site link token")
	}
	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name != controllerPluginName && mountNamed(c.VolumeMounts, siteLinkTokenVolumeName) != nil {
			t.Errorf("sidecar %s mounts the site link token", c.Name)
		}
	}
}

func TestTheControllerIsUnchangedWithoutASiteLink(t *testing.T) {
	sts := withSiteLink(controllerStatefulSet(testDriver("d"), "image"), nil)
	if volumeNamed(sts.Spec.Template.Spec.Volumes, siteLinkTokenVolumeName) != nil {
		t.Error("a controller without a site link got its token")
	}
}
