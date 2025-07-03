package kuberesolver

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeClientConn struct {
	state     resolver.State
	addresses []string
	updated   chan struct{}
}

func (fc *fakeClientConn) UpdateState(state resolver.State) error {
	fc.state = state
	fc.addresses = nil
	for _, addr := range state.Addresses {
		fc.addresses = append(fc.addresses, addr.Addr)
	}
	if fc.updated != nil {
		select {
		case fc.updated <- struct{}{}:
		default:
		}
	}
	return nil
}

func (fc *fakeClientConn) ReportError(err error) {}

func (fc *fakeClientConn) NewAddress(addresses []resolver.Address) {}

func (fc *fakeClientConn) NewServiceConfig(serviceConfig string) {}

func (fc *fakeClientConn) ParseServiceConfig(serviceConfigJSON string) *serviceconfig.ParseResult {
	return &serviceconfig.ParseResult{}
}

func parseTarget(target string) resolver.Target {
	u, err := url.Parse(target)
	if err != nil {
		panic(err)
	}
	return resolver.Target{URL: *u}
}

func createTestEndpointSlice(name, namespace, serviceName string, addresses []string, port int32) *discoveryv1.EndpointSlice {
	ready := true
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name": serviceName,
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses: addresses,
				Conditions: discoveryv1.EndpointConditions{
					Ready: &ready,
				},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{
				Port: &port,
			},
		},
	}
}

func TestParseResolverTarget(t *testing.T) {
	tests := []struct {
		target resolver.Target
		want   targetInfo
		err    bool
	}{
		{parseTarget("/"), targetInfo{"", "", "", "", false, false}, true},
		{parseTarget("a"), targetInfo{"", "a", "", "", false, true}, false},
		{parseTarget("/a"), targetInfo{"", "a", "", "", false, true}, false},
		{parseTarget("//a/b"), targetInfo{"", "b", "a", "", false, true}, false},
		{parseTarget("a.b"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("/a.b"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("/a.b:80"), targetInfo{"", "a", "b", "80", false, false}, false},
		{parseTarget("/a.b:port"), targetInfo{"", "a", "b", "port", true, false}, false},
		{parseTarget("//a/b:port"), targetInfo{"", "b", "a", "port", true, false}, false},
		{parseTarget("//a/b:80"), targetInfo{"", "b", "a", "80", false, false}, false},
		{parseTarget("a.b.svc.cluster.local"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("/a.b.svc.cluster.local:80"), targetInfo{"", "a", "b", "80", false, false}, false},
		{parseTarget("/a.b.svc.cluster.local:port"), targetInfo{"", "a", "b", "port", true, false}, false},
		{parseTarget("//a.b.svc.cluster.local"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("//a.b.svc.cluster.local:80"), targetInfo{"", "a", "b", "80", false, false}, false},
	}

	for i, test := range tests {
		got, err := parseResolverTarget(test.target)
		if test.err {
			assert.Error(t, err, "case %d: expected error", i)
		} else {
			assert.NoError(t, err, "case %d: unexpected error", i)
			assert.Equal(t, test.want, got, "case %d: parseResolverTarget(%q)", i, &test.target.URL)
		}
	}
}

func TestParseTargets(t *testing.T) {
	tests := []struct {
		target string
		want   targetInfo
		err    bool
	}{
		{"", targetInfo{}, true},
		{"kubernetes:///", targetInfo{}, true},
		{"kubernetes://a:30", targetInfo{"kubernetes", "a", "", "30", false, false}, false},
		{"kubernetes://a/", targetInfo{"kubernetes", "a", "", "", false, true}, false},
		{"kubernetes:///a", targetInfo{"kubernetes", "a", "", "", false, true}, false},
		{"kubernetes://a/b", targetInfo{"kubernetes", "b", "a", "", false, true}, false},
		{"kubernetes://a.b/", targetInfo{"kubernetes", "a", "b", "", false, true}, false},
		{"kubernetes:///a.b:80", targetInfo{"kubernetes", "a", "b", "80", false, false}, false},
		{"kubernetes:///a.b:port", targetInfo{"kubernetes", "a", "b", "port", true, false}, false},
		{"kubernetes:///a:port", targetInfo{"kubernetes", "a", "", "port", true, false}, false},
		{"kubernetes://x/a:port", targetInfo{"kubernetes", "a", "x", "port", true, false}, false},
		{"kubernetes://a.x:30/", targetInfo{"kubernetes", "a", "x", "30", false, false}, false},
		{"kubernetes://a.b.svc.cluster.local", targetInfo{"kubernetes", "a", "b", "", false, true}, false},
		{"kubernetes://a.b.svc.cluster.local:80", targetInfo{"kubernetes", "a", "b", "80", false, false}, false},
		{"kubernetes:///a.b.svc.cluster.local", targetInfo{"kubernetes", "a", "b", "", false, true}, false},
		{"kubernetes:///a.b.svc.cluster.local:80", targetInfo{"kubernetes", "a", "b", "80", false, false}, false},
		{"kubernetes:///a.b.svc.cluster.local:port", targetInfo{"kubernetes", "a", "b", "port", true, false}, false},
	}

	for i, test := range tests {
		got, err := parseResolverTarget(parseTarget(test.target))
		if test.err {
			assert.Error(t, err, "case %d: expected error", i)
		} else {
			assert.NoError(t, err, "case %d: unexpected error", i)
			assert.Equal(t, test.want, got, "case %d: parseTarget(%q)", i, test.target)
		}
	}
}

func TestBuilderWithInformer(t *testing.T) {
	// Create fake client with test data
	endpointSlice := createTestEndpointSlice(
		"test-service-abc123",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.1", "10.0.0.2"},
		8080,
	)

	fakeClient := fake.NewSimpleClientset(endpointSlice)

	// Create builder
	builder := NewBuilder(fakeClient, kubernetesSchema)

	// Create fake connection
	conn := &fakeClientConn{
		updated: make(chan struct{}, 1),
	}

	// Build resolver
	target := parseTarget("kubernetes:///test-service.test-namespace:8080")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Check addresses
	expectedAddresses := []string{"10.0.0.1:8080", "10.0.0.2:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestBuilderWithMultipleEndpointSlices(t *testing.T) {
	// Create multiple EndpointSlices for the same service
	endpointSlice1 := createTestEndpointSlice(
		"test-service-abc123",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.1", "10.0.0.2"},
		8080,
	)

	endpointSlice2 := createTestEndpointSlice(
		"test-service-def456",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.3", "10.0.0.4"},
		8080,
	)

	fakeClient := fake.NewSimpleClientset(endpointSlice1, endpointSlice2)

	builder := NewBuilder(fakeClient, kubernetesSchema)
	conn := &fakeClientConn{
		updated: make(chan struct{}, 1),
	}

	target := parseTarget("kubernetes:///test-service.test-namespace:8080")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Check that all addresses from both slices are present
	expectedAddresses := []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080", "10.0.0.4:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestBuilderWithPortName(t *testing.T) {
	// Create EndpointSlice with named port
	ready := true
	portName := "http"
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc123",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-service",
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses: []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{
					Ready: &ready,
				},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{
				Name: &portName,
				Port: &[]int32{8080}[0],
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(endpointSlice)

	builder := NewBuilder(fakeClient, kubernetesSchema)
	conn := &fakeClientConn{
		updated: make(chan struct{}, 1),
	}

	// Test resolving by port name
	target := parseTarget("kubernetes:///test-service.test-namespace:http")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Check addresses
	expectedAddresses := []string{"10.0.0.1:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestBuilderWithDynamicUpdates(t *testing.T) {
	// Start with one EndpointSlice
	endpointSlice := createTestEndpointSlice(
		"test-service-abc123",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.1"},
		8080,
	)

	fakeClient := fake.NewSimpleClientset(endpointSlice)

	builder := NewBuilder(fakeClient, kubernetesSchema)
	conn := &fakeClientConn{
		updated: make(chan struct{}, 10),
	}

	target := parseTarget("kubernetes:///test-service.test-namespace:8080")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Check initial addresses
	expectedAddresses := []string{"10.0.0.1:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)

	// Create another EndpointSlice by adding it to the fake client
	// The informer should automatically pick this up
	newEndpointSlice := createTestEndpointSlice(
		"test-service-def456",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.2"},
		8080,
	)

	// Add the new EndpointSlice through the fake client
	// This should trigger the informer automatically
	_, err = fakeClient.DiscoveryV1().EndpointSlices("test-namespace").Create(
		context.Background(),
		newEndpointSlice,
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	// Give the informer a moment to process the event
	time.Sleep(100 * time.Millisecond)

	// Wait for update
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for update after adding EndpointSlice")
	}

	// Check updated addresses - should now have both endpoints
	expectedAddresses = []string{"10.0.0.1:8080", "10.0.0.2:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestBuilderWithEndpointSliceUpdate(t *testing.T) {
	// Start with one EndpointSlice with one endpoint
	endpointSlice := createTestEndpointSlice(
		"test-service-abc123",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.1"},
		8080,
	)

	fakeClient := fake.NewSimpleClientset(endpointSlice)

	builder := NewBuilder(fakeClient, kubernetesSchema)
	conn := &fakeClientConn{
		updated: make(chan struct{}, 10),
	}

	target := parseTarget("kubernetes:///test-service.test-namespace:8080")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Check initial addresses
	expectedAddresses := []string{"10.0.0.1:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)

	// Update the EndpointSlice to add another endpoint
	updatedEndpointSlice := endpointSlice.DeepCopy()
	ready := true
	updatedEndpointSlice.Endpoints = append(updatedEndpointSlice.Endpoints, discoveryv1.Endpoint{
		Addresses: []string{"10.0.0.2"},
		Conditions: discoveryv1.EndpointConditions{
			Ready: &ready,
		},
	})

	// Update the EndpointSlice through the fake client
	// This should trigger the informer automatically
	_, err = fakeClient.DiscoveryV1().EndpointSlices("test-namespace").Update(
		context.Background(),
		updatedEndpointSlice,
		metav1.UpdateOptions{},
	)
	require.NoError(t, err)

	// Give the informer a moment to process the event
	time.Sleep(100 * time.Millisecond)

	// Wait for update
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for update after modifying EndpointSlice")
	}

	// Check updated addresses - should now have both endpoints
	expectedAddresses = []string{"10.0.0.1:8080", "10.0.0.2:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestBuilderWithEndpointSliceDelete(t *testing.T) {
	// Start with two EndpointSlices
	endpointSlice1 := createTestEndpointSlice(
		"test-service-abc123",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.1"},
		8080,
	)

	endpointSlice2 := createTestEndpointSlice(
		"test-service-def456",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.2"},
		8080,
	)

	fakeClient := fake.NewSimpleClientset(endpointSlice1, endpointSlice2)

	builder := NewBuilder(fakeClient, kubernetesSchema)
	conn := &fakeClientConn{
		updated: make(chan struct{}, 10),
	}

	target := parseTarget("kubernetes:///test-service.test-namespace:8080")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Check initial addresses - should have both endpoints
	expectedAddresses := []string{"10.0.0.1:8080", "10.0.0.2:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)

	// Delete one EndpointSlice through the fake client
	// This should trigger the informer automatically
	err = fakeClient.DiscoveryV1().EndpointSlices("test-namespace").Delete(
		context.Background(),
		endpointSlice2.Name,
		metav1.DeleteOptions{},
	)
	require.NoError(t, err)

	// Give the informer a moment to process the event
	time.Sleep(100 * time.Millisecond)

	// Wait for update
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for update after deleting EndpointSlice")
	}

	// Check updated addresses - should now have only one endpoint
	expectedAddresses = []string{"10.0.0.1:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestResolveNow(t *testing.T) {
	endpointSlice := createTestEndpointSlice(
		"test-service-abc123",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.1"},
		8080,
	)

	fakeClient := fake.NewSimpleClientset(endpointSlice)

	builder := NewBuilder(fakeClient, kubernetesSchema)
	conn := &fakeClientConn{
		updated: make(chan struct{}, 10),
	}

	target := parseTarget("kubernetes:///test-service.test-namespace:8080")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Call ResolveNow - should trigger another resolution
	res.ResolveNow(resolver.ResolveNowOptions{})

	// Should get another update
	select {
	case <-conn.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ResolveNow update")
	}

	expectedAddresses := []string{"10.0.0.1:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestInformerEventHandlers(t *testing.T) {
	// Test that the informer event handlers are properly set up and working
	endpointSlice := createTestEndpointSlice(
		"test-service-abc123",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.1"},
		8080,
	)

	fakeClient := fake.NewSimpleClientset(endpointSlice)

	builder := NewBuilder(fakeClient, kubernetesSchema)
	conn := &fakeClientConn{
		updated: make(chan struct{}, 10),
	}

	target := parseTarget("kubernetes:///test-service.test-namespace:8080")
	res, err := builder.Build(target, conn, resolver.BuildOptions{})
	require.NoError(t, err)
	defer res.Close()

	// Wait for initial resolution
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial resolution")
	}

	// Check initial addresses
	expectedAddresses := []string{"10.0.0.1:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)

	// Test ADD event handler by creating a new EndpointSlice
	newEndpointSlice := createTestEndpointSlice(
		"test-service-def456",
		"test-namespace",
		"test-service",
		[]string{"10.0.0.2"},
		8080,
	)

	// Add through the fake client - this should trigger the informer event handler
	_, err = fakeClient.DiscoveryV1().EndpointSlices("test-namespace").Create(
		context.Background(),
		newEndpointSlice,
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	// Give the informer a moment to process the event
	time.Sleep(100 * time.Millisecond)

	// Wait for update
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for update after adding EndpointSlice")
	}

	// Check updated addresses
	expectedAddresses = []string{"10.0.0.1:8080", "10.0.0.2:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)

	// Test UPDATE event handler
	updatedEndpointSlice := newEndpointSlice.DeepCopy()
	ready := true
	updatedEndpointSlice.Endpoints = append(updatedEndpointSlice.Endpoints, discoveryv1.Endpoint{
		Addresses: []string{"10.0.0.3"},
		Conditions: discoveryv1.EndpointConditions{
			Ready: &ready,
		},
	})

	// Update through the fake client
	_, err = fakeClient.DiscoveryV1().EndpointSlices("test-namespace").Update(
		context.Background(),
		updatedEndpointSlice,
		metav1.UpdateOptions{},
	)
	require.NoError(t, err)

	// Give the informer a moment to process the event
	time.Sleep(100 * time.Millisecond)

	// Wait for update
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for update after updating EndpointSlice")
	}

	// Check updated addresses
	expectedAddresses = []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)

	// Test DELETE event handler
	err = fakeClient.DiscoveryV1().EndpointSlices("test-namespace").Delete(
		context.Background(),
		updatedEndpointSlice.Name,
		metav1.DeleteOptions{},
	)
	require.NoError(t, err)

	// Give the informer a moment to process the event
	time.Sleep(100 * time.Millisecond)

	// Wait for update
	select {
	case <-conn.updated:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for update after deleting EndpointSlice")
	}

	// Check updated addresses - should only have the original endpoint
	expectedAddresses = []string{"10.0.0.1:8080"}
	assert.ElementsMatch(t, expectedAddresses, conn.addresses)
}

func TestIsRelevantEndpointSlice(t *testing.T) {
	// Create a resolver for testing
	target := targetInfo{
		serviceName:      "test-service",
		serviceNamespace: "test-namespace",
	}

	kRes := &kResolver{
		target: target,
	}

	// Test relevant EndpointSlice
	relevantSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc123",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-service",
			},
		},
	}
	assert.True(t, kRes.isRelevantEndpointSlice(relevantSlice))

	// Test EndpointSlice with wrong namespace
	wrongNamespaceSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc123",
			Namespace: "wrong-namespace",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-service",
			},
		},
	}
	assert.False(t, kRes.isRelevantEndpointSlice(wrongNamespaceSlice))

	// Test EndpointSlice with wrong service name
	wrongServiceSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-service-abc123",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"kubernetes.io/service-name": "wrong-service",
			},
		},
	}
	assert.False(t, kRes.isRelevantEndpointSlice(wrongServiceSlice))

	// Test EndpointSlice without service name label
	noLabelSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc123",
			Namespace: "test-namespace",
			Labels:    map[string]string{},
		},
	}
	assert.False(t, kRes.isRelevantEndpointSlice(noLabelSlice))

	// Test EndpointSlice with nil labels
	nilLabelSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc123",
			Namespace: "test-namespace",
			Labels:    nil,
		},
	}
	assert.False(t, kRes.isRelevantEndpointSlice(nilLabelSlice))
}

func TestGetCurrentNamespaceOrDefault(t *testing.T) {
	// Test that the function returns a valid namespace (either from environment or default)
	namespace := getCurrentNamespaceOrDefault()

	// Should return a non-empty string
	assert.NotEmpty(t, namespace)

	// Should be a valid Kubernetes namespace name (basic check)
	assert.Regexp(t, `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, namespace)

	// In test environment, it should typically return "default"
	// unless running in a cluster or with specific kubeconfig
	t.Logf("Detected namespace: %s", namespace)
}

func TestSplitServicePortNamespace(t *testing.T) {
	tests := []struct {
		input             string
		expectedService   string
		expectedPort      string
		expectedNamespace string
	}{
		{"service", "service", "", ""},
		{"service:8080", "service", "8080", ""},
		{"service.namespace", "service", "", "namespace"},
		{"service.namespace:8080", "service", "8080", "namespace"},
		{"service.namespace.svc.cluster.local", "service", "", "namespace"},
		{"service.namespace.svc.cluster.local:8080", "service", "8080", "namespace"},
	}

	for _, test := range tests {
		service, port, namespace := splitServicePortNamespace(test.input)
		assert.Equal(t, test.expectedService, service, "service for input %q", test.input)
		assert.Equal(t, test.expectedPort, port, "port for input %q", test.input)
		assert.Equal(t, test.expectedNamespace, namespace, "namespace for input %q", test.input)
	}
}
