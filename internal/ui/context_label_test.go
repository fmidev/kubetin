package ui

import (
	"strings"
	"testing"

	"github.com/fmidev/kubetin/internal/model"
)

func TestDisambiguatedContextLabelsRemainVisible(t *testing.T) {
	for _, name := range []string{"prod (a)", "prod (b)"} {
		m := New(name, model.NewStore(), []string{"prod (a)", "prod (b)"})
		m.width, m.height = 200, 40
		st := model.ClusterState{Context: name, RawName: "prod", Reach: model.ReachHealthy}
		for area, rendered := range map[string]string{
			"header":  m.renderHeaderIdentity(st),
			"sidebar": m.renderSidebarRow(st),
			"fleet":   m.renderFleetCompactRow(st, 160),
		} {
			if !strings.Contains(rendered, name) {
				t.Errorf("%s hides the disambiguated name %q: %s", area, name, rendered)
			}
		}
		st.Reach = model.ReachUnreachable
		if rendered := m.renderFleetCompactRow(st, 160); !strings.Contains(rendered, name) {
			t.Errorf("offline fleet row hides the disambiguated name %q: %s", name, rendered)
		}
	}
}
