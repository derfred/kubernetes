/*
Copyright 2014 Google Inc. All rights reserved.

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

package apiserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/admission"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	apierrs "github.com/GoogleCloudPlatform/kubernetes/pkg/api/errors"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/meta"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/version"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
	"github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/admission/admit"
	"github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/admission/deny"

	"github.com/emicklei/go-restful"
)

func convert(obj runtime.Object) (runtime.Object, error) {
	return obj, nil
}

// This creates a fake API version, similar to api/latest.go
const testVersion = "version"

var versions = []string{testVersion}
var codec = runtime.CodecFor(api.Scheme, testVersion)
var accessor = meta.NewAccessor()
var versioner runtime.ResourceVersioner = accessor
var selfLinker runtime.SelfLinker = accessor
var mapper, namespaceMapper, legacyNamespaceMapper meta.RESTMapper // The mappers with namespace and with legacy namespace scopes.
var admissionControl admission.Interface
var requestContextMapper api.RequestContextMapper

func interfacesFor(version string) (*meta.VersionInterfaces, error) {
	switch version {
	case testVersion:
		return &meta.VersionInterfaces{
			Codec:            codec,
			ObjectConvertor:  api.Scheme,
			MetadataAccessor: accessor,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported storage version: %s (valid: %s)", version, strings.Join(versions, ", "))
	}
}

func newMapper() *meta.DefaultRESTMapper {
	return meta.NewDefaultRESTMapper(
		versions,
		func(version string) (*meta.VersionInterfaces, bool) {
			interfaces, err := interfacesFor(version)
			if err != nil {
				return nil, false
			}
			return interfaces, true
		},
	)
}

func init() {
	// Certain API objects are returned regardless of the contents of storage:
	// api.Status is returned in errors

	// "internal" version
	api.Scheme.AddKnownTypes("", &Simple{}, &SimpleList{},
		&api.Status{})
	// "version" version
	// TODO: Use versioned api objects?
	api.Scheme.AddKnownTypes(testVersion, &Simple{}, &SimpleList{},
		&api.Status{})

	nsMapper := newMapper()
	legacyNsMapper := newMapper()
	// enumerate all supported versions, get the kinds, and register with the mapper how to address our resources
	for _, version := range versions {
		for kind := range api.Scheme.KnownTypes(version) {
			mixedCase := true
			legacyNsMapper.Add(meta.RESTScopeNamespaceLegacy, kind, version, mixedCase)
			nsMapper.Add(meta.RESTScopeNamespace, kind, version, mixedCase)
		}
	}

	mapper = legacyNsMapper
	legacyNamespaceMapper = legacyNsMapper
	namespaceMapper = nsMapper
	admissionControl = admit.NewAlwaysAdmit()
	requestContextMapper = api.NewRequestContextMapper()
}

// defaultAPIServer exposes nested objects for testability.
type defaultAPIServer struct {
	http.Handler
	group *APIGroupVersion
}

// uses the default settings
func handle(storage map[string]RESTStorage) http.Handler {
	return handleInternal(storage, admissionControl, mapper, selfLinker)
}

// tests with a deny admission controller
func handleDeny(storage map[string]RESTStorage) http.Handler {
	return handleInternal(storage, deny.NewAlwaysDeny(), mapper, selfLinker)
}

// tests using the new namespace scope mechanism
func handleNamespaced(storage map[string]RESTStorage) http.Handler {
	return handleInternal(storage, admissionControl, namespaceMapper, selfLinker)
}

// tests using a custom self linker
func handleLinker(storage map[string]RESTStorage, selfLinker runtime.SelfLinker) http.Handler {
	return handleInternal(storage, admissionControl, mapper, selfLinker)
}

func handleInternal(storage map[string]RESTStorage, admissionControl admission.Interface, mapper meta.RESTMapper, selfLinker runtime.SelfLinker) http.Handler {
	group := &APIGroupVersion{
		Storage: storage,

		Mapper: mapper,

		Root:    "/api",
		Version: testVersion,

		Creater: api.Scheme,
		Typer:   api.Scheme,
		Codec:   codec,
		Linker:  selfLinker,

		Admit:   admissionControl,
		Context: requestContextMapper,
	}
	container := restful.NewContainer()
	container.Router(restful.CurlyRouter{})
	mux := container.ServeMux
	group.InstallREST(container)
	ws := new(restful.WebService)
	InstallSupport(mux, ws)
	container.Add(ws)
	return &defaultAPIServer{mux, group}
}

type Simple struct {
	api.TypeMeta   `json:",inline"`
	api.ObjectMeta `json:"metadata"`
	Other          string `json:"other,omitempty"`
}

func (*Simple) IsAnAPIObject() {}

type SimpleList struct {
	api.TypeMeta `json:",inline"`
	api.ListMeta `json:"metadata,inline"`
	Items        []Simple `json:"items,omitempty"`
}

func (*SimpleList) IsAnAPIObject() {}

func TestSimpleSetupRight(t *testing.T) {
	s := &Simple{ObjectMeta: api.ObjectMeta{Name: "aName"}}
	wire, err := codec.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := codec.Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, s2) {
		t.Fatalf("encode/decode broken:\n%#v\n%#v\n", s, s2)
	}
}

type SimpleRESTStorage struct {
	errors  map[string]error
	list    []Simple
	item    Simple
	deleted string
	updated *Simple
	created *Simple

	actualNamespace  string
	namespacePresent bool

	// These are set when Watch is called
	fakeWatch                  *watch.FakeWatcher
	requestedLabelSelector     labels.Selector
	requestedFieldSelector     labels.Selector
	requestedResourceVersion   string
	requestedResourceNamespace string

	// The id requested, and location to return for ResourceLocation
	requestedResourceLocationID string
	resourceLocation            string
	expectedResourceNamespace   string

	// If non-nil, called inside the WorkFunc when answering update, delete, create.
	// obj receives the original input to the update, delete, or create call.
	injectedFunction func(obj runtime.Object) (returnObj runtime.Object, err error)
}

func (storage *SimpleRESTStorage) List(ctx api.Context, label, field labels.Selector) (runtime.Object, error) {
	storage.checkContext(ctx)
	result := &SimpleList{
		Items: storage.list,
	}
	return result, storage.errors["list"]
}

func (storage *SimpleRESTStorage) Get(ctx api.Context, id string) (runtime.Object, error) {
	storage.checkContext(ctx)
	return api.Scheme.CopyOrDie(&storage.item), storage.errors["get"]
}

func (storage *SimpleRESTStorage) checkContext(ctx api.Context) {
	storage.actualNamespace, storage.namespacePresent = api.NamespaceFrom(ctx)
}

func (storage *SimpleRESTStorage) Delete(ctx api.Context, id string) (runtime.Object, error) {
	storage.checkContext(ctx)
	storage.deleted = id
	if err := storage.errors["delete"]; err != nil {
		return nil, err
	}
	var obj runtime.Object = &api.Status{Status: api.StatusSuccess}
	var err error
	if storage.injectedFunction != nil {
		obj, err = storage.injectedFunction(&Simple{ObjectMeta: api.ObjectMeta{Name: id}})
	}
	return obj, err
}

func (storage *SimpleRESTStorage) New() runtime.Object {
	return &Simple{}
}

func (storage *SimpleRESTStorage) NewList() runtime.Object {
	return &SimpleList{}
}

func (storage *SimpleRESTStorage) Create(ctx api.Context, obj runtime.Object) (runtime.Object, error) {
	storage.checkContext(ctx)
	storage.created = obj.(*Simple)
	if err := storage.errors["create"]; err != nil {
		return nil, err
	}
	var err error
	if storage.injectedFunction != nil {
		obj, err = storage.injectedFunction(obj)
	}
	return obj, err
}

func (storage *SimpleRESTStorage) Update(ctx api.Context, obj runtime.Object) (runtime.Object, bool, error) {
	storage.checkContext(ctx)
	storage.updated = obj.(*Simple)
	if err := storage.errors["update"]; err != nil {
		return nil, false, err
	}
	var err error
	if storage.injectedFunction != nil {
		obj, err = storage.injectedFunction(obj)
	}
	return obj, false, err
}

// Implement ResourceWatcher.
func (storage *SimpleRESTStorage) Watch(ctx api.Context, label, field labels.Selector, resourceVersion string) (watch.Interface, error) {
	storage.checkContext(ctx)
	storage.requestedLabelSelector = label
	storage.requestedFieldSelector = field
	storage.requestedResourceVersion = resourceVersion
	storage.requestedResourceNamespace = api.NamespaceValue(ctx)
	if err := storage.errors["watch"]; err != nil {
		return nil, err
	}
	storage.fakeWatch = watch.NewFake()
	return storage.fakeWatch, nil
}

// Implement Redirector.
func (storage *SimpleRESTStorage) ResourceLocation(ctx api.Context, id string) (string, error) {
	storage.checkContext(ctx)
	// validate that the namespace context on the request matches the expected input
	storage.requestedResourceNamespace = api.NamespaceValue(ctx)
	if storage.expectedResourceNamespace != storage.requestedResourceNamespace {
		return "", fmt.Errorf("Expected request namespace %s, but got namespace %s", storage.expectedResourceNamespace, storage.requestedResourceNamespace)
	}
	storage.requestedResourceLocationID = id
	if err := storage.errors["resourceLocation"]; err != nil {
		return "", err
	}
	return storage.resourceLocation, nil
}

func extractBody(response *http.Response, object runtime.Object) (string, error) {
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return string(body), err
	}
	err = codec.DecodeInto(body, object)
	return string(body), err
}

func TestNotFound(t *testing.T) {
	type T struct {
		Method string
		Path   string
		Status int
	}
	cases := map[string]T{
		"PATCH method":                 {"PATCH", "/api/version/foo", http.StatusMethodNotAllowed},
		"GET long prefix":              {"GET", "/api/", http.StatusNotFound},
		"GET missing storage":          {"GET", "/api/version/blah", http.StatusNotFound},
		"GET with extra segment":       {"GET", "/api/version/foo/bar/baz", http.StatusNotFound},
		"POST with extra segment":      {"POST", "/api/version/foo/bar", http.StatusMethodNotAllowed},
		"DELETE without extra segment": {"DELETE", "/api/version/foo", http.StatusMethodNotAllowed},
		"DELETE with extra segment":    {"DELETE", "/api/version/foo/bar/baz", http.StatusNotFound},
		"PUT without extra segment":    {"PUT", "/api/version/foo", http.StatusMethodNotAllowed},
		"PUT with extra segment":       {"PUT", "/api/version/foo/bar/baz", http.StatusNotFound},
		"watch missing storage":        {"GET", "/api/version/watch/", http.StatusNotFound},
		"watch with bad method":        {"POST", "/api/version/watch/foo/bar", http.StatusMethodNotAllowed},
	}
	handler := handle(map[string]RESTStorage{
		"foo": &SimpleRESTStorage{},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}
	for k, v := range cases {
		request, err := http.NewRequest(v.Method, server.URL+v.Path, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		response, err := client.Do(request)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if response.StatusCode != v.Status {
			t.Errorf("Expected %d for %s (%s), Got %#v", v.Status, v.Method, k, response)
			t.Errorf("MAPPER: %v", mapper)
		}
	}
}

type UnimplementedRESTStorage struct{}

func (UnimplementedRESTStorage) New() runtime.Object {
	return &Simple{}
}

// TestUnimplementedRESTStorage ensures that if a RESTStorage does not implement a given
// method, that it is literally not registered with the server.  In the past,
// we registered everything, and returned method not supported if it didn't support
// a verb.  Now we literally do not register a storage if it does not implement anything.
// TODO: in future, we should update proxy/redirect
func TestUnimplementedRESTStorage(t *testing.T) {
	type T struct {
		Method  string
		Path    string
		ErrCode int
	}
	cases := map[string]T{
		"GET object":      {"GET", "/api/version/foo/bar", http.StatusNotFound},
		"GET list":        {"GET", "/api/version/foo", http.StatusNotFound},
		"POST list":       {"POST", "/api/version/foo", http.StatusNotFound},
		"PUT object":      {"PUT", "/api/version/foo/bar", http.StatusNotFound},
		"DELETE object":   {"DELETE", "/api/version/foo/bar", http.StatusNotFound},
		"watch list":      {"GET", "/api/version/watch/foo", http.StatusNotFound},
		"watch object":    {"GET", "/api/version/watch/foo/bar", http.StatusNotFound},
		"proxy object":    {"GET", "/api/version/proxy/foo/bar", http.StatusNotFound},
		"redirect object": {"GET", "/api/version/redirect/foo/bar", http.StatusNotFound},
	}
	handler := handle(map[string]RESTStorage{
		"foo": UnimplementedRESTStorage{},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}
	for k, v := range cases {
		request, err := http.NewRequest(v.Method, server.URL+v.Path, bytes.NewReader([]byte(`{"kind":"Simple","apiVersion":"version"}`)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
			continue
		}
		defer response.Body.Close()
		data, _ := ioutil.ReadAll(response.Body)
		if response.StatusCode != v.ErrCode {
			t.Errorf("%s: expected %d for %s, Got %s", k, v.ErrCode, v.Method, string(data))
			continue
		}
	}
}

func TestVersion(t *testing.T) {
	handler := handle(map[string]RESTStorage{})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	request, err := http.NewRequest("GET", server.URL+"/version", nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	var info version.Info
	err = json.NewDecoder(response.Body).Decode(&info)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(version.Get(), info) {
		t.Errorf("Expected %#v, Got %#v", version.Get(), info)
	}
}

func TestList(t *testing.T) {
	testCases := []struct {
		url       string
		namespace string
		selfLink  string
		legacy    bool
	}{
		{"/api/version/simple", "", "/api/version/simple?namespace=", true},
		{"/api/version/simple?namespace=other", "other", "/api/version/simple?namespace=other", true},
		// list items across all namespaces
		{"/api/version/simple?namespace=", "", "/api/version/simple?namespace=", true},
		{"/api/version/namespaces/default/simple", "default", "/api/version/namespaces/default/simple", false},
		{"/api/version/namespaces/other/simple", "other", "/api/version/namespaces/other/simple", false},
		// list items across all namespaces
		{"/api/version/simple", "", "/api/version/simple", false},
	}
	for i, testCase := range testCases {
		storage := map[string]RESTStorage{}
		simpleStorage := SimpleRESTStorage{expectedResourceNamespace: testCase.namespace}
		storage["simple"] = &simpleStorage
		selfLinker := &setTestSelfLinker{
			t:           t,
			namespace:   testCase.namespace,
			expectedSet: testCase.selfLink,
		}
		var handler http.Handler
		if testCase.legacy {
			handler = handleLinker(storage, selfLinker)
		} else {
			handler = handleInternal(storage, admissionControl, namespaceMapper, selfLinker)
		}
		server := httptest.NewServer(handler)
		defer server.Close()

		resp, err := http.Get(server.URL + testCase.url)
		if err != nil {
			t.Errorf("%d: unexpected error: %v", i, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%d: unexpected status: %d, Expected: %d, %#v", i, resp.StatusCode, http.StatusOK, resp)
		}
		// TODO: future, restore get links
		if !selfLinker.called {
			t.Errorf("%d: never set self link", i)
		}
		if !simpleStorage.namespacePresent {
			t.Errorf("%d: namespace not set", i)
		} else if simpleStorage.actualNamespace != testCase.namespace {
			t.Errorf("%d: unexpected resource namespace: %s", i, simpleStorage.actualNamespace)
		}
	}
}

func TestErrorList(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"list": fmt.Errorf("test Error")},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/version/simple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", resp.StatusCode, http.StatusInternalServerError, resp)
	}
}

func TestNonEmptyList(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{
		list: []Simple{
			{
				ObjectMeta: api.ObjectMeta{Name: "something", Namespace: "other"},
				Other:      "foo",
			},
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/version/simple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", resp.StatusCode, http.StatusOK, resp)
		body, _ := ioutil.ReadAll(resp.Body)
		t.Logf("Data: %s", string(body))
	}

	var listOut SimpleList
	body, err := extractBody(resp, &listOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(listOut.Items) != 1 {
		t.Errorf("Unexpected response: %#v", listOut)
		return
	}
	if listOut.Items[0].Other != simpleStorage.list[0].Other {
		t.Errorf("Unexpected data: %#v, %s", listOut.Items[0], string(body))
	}
	if listOut.SelfLink != "/api/version/simple?namespace=" {
		t.Errorf("unexpected list self link: %#v", listOut)
	}
	expectedSelfLink := "/api/version/simple/something?namespace=other"
	if listOut.Items[0].ObjectMeta.SelfLink != expectedSelfLink {
		t.Errorf("Unexpected data: %#v, %s", listOut.Items[0].ObjectMeta.SelfLink, expectedSelfLink)
	}
}

func TestSelfLinkSkipsEmptyName(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{
		list: []Simple{
			{
				ObjectMeta: api.ObjectMeta{Namespace: "other"},
				Other:      "foo",
			},
		},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/version/simple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", resp.StatusCode, http.StatusOK, resp)
		body, _ := ioutil.ReadAll(resp.Body)
		t.Logf("Data: %s", string(body))
	}
	var listOut SimpleList
	body, err := extractBody(resp, &listOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(listOut.Items) != 1 {
		t.Errorf("Unexpected response: %#v", listOut)
		return
	}
	if listOut.Items[0].Other != simpleStorage.list[0].Other {
		t.Errorf("Unexpected data: %#v, %s", listOut.Items[0], string(body))
	}
	if listOut.SelfLink != "/api/version/simple?namespace=" {
		t.Errorf("unexpected list self link: %#v", listOut)
	}
	expectedSelfLink := ""
	if listOut.Items[0].ObjectMeta.SelfLink != expectedSelfLink {
		t.Errorf("Unexpected data: %#v, %s", listOut.Items[0].ObjectMeta.SelfLink, expectedSelfLink)
	}
}

func TestGet(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{
		item: Simple{
			Other: "foo",
		},
	}
	selfLinker := &setTestSelfLinker{
		t:           t,
		expectedSet: "/api/version/simple/id?namespace=default",
		name:        "id",
		namespace:   "default",
	}
	storage["simple"] = &simpleStorage
	handler := handleLinker(storage, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/version/simple/id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: %#v", resp)
	}
	var itemOut Simple
	body, err := extractBody(resp, &itemOut)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if itemOut.Name != simpleStorage.item.Name {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simpleStorage.item, string(body))
	}
	if !selfLinker.called {
		t.Errorf("Never set self link")
	}
}

func TestGetAlternateSelfLink(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{
		item: Simple{
			Other: "foo",
		},
	}
	selfLinker := &setTestSelfLinker{
		t:           t,
		expectedSet: "/api/version/simple/id?namespace=test",
		name:        "id",
		namespace:   "test",
	}
	storage["simple"] = &simpleStorage
	handler := handleLinker(storage, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/version/simple/id?namespace=test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: %#v", resp)
	}
	var itemOut Simple
	body, err := extractBody(resp, &itemOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if itemOut.Name != simpleStorage.item.Name {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simpleStorage.item, string(body))
	}
	if !selfLinker.called {
		t.Errorf("Never set self link")
	}
}

func TestGetNamespaceSelfLink(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{
		item: Simple{
			Other: "foo",
		},
	}
	selfLinker := &setTestSelfLinker{
		t:           t,
		expectedSet: "/api/version/namespaces/foo/simple/id",
		name:        "id",
		namespace:   "foo",
	}
	storage["simple"] = &simpleStorage
	handler := handleInternal(storage, admissionControl, namespaceMapper, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/version/namespaces/foo/simple/id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: %#v", resp)
	}
	var itemOut Simple
	body, err := extractBody(resp, &itemOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if itemOut.Name != simpleStorage.item.Name {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simpleStorage.item, string(body))
	}
	if !selfLinker.called {
		t.Errorf("Never set self link")
	}
}
func TestGetMissing(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"get": apierrs.NewNotFound("simple", "id")},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/version/simple/id")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", resp)
	}
}

func TestDelete(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/api/version/simple/"+ID, nil)
	res, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("unexpected response: %#v", res)
	}
	if simpleStorage.deleted != ID {
		t.Errorf("Unexpected delete: %s, expected %s", simpleStorage.deleted, ID)
	}
}

func TestDeleteInvokesAdmissionControl(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handleDeny(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/api/version/simple/"+ID, nil)
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestDeleteMissing(t *testing.T) {
	storage := map[string]RESTStorage{}
	ID := "id"
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"delete": apierrs.NewNotFound("simple", ID)},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := http.Client{}
	request, err := http.NewRequest("DELETE", server.URL+"/api/version/simple/"+ID, nil)
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestUpdate(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	selfLinker := &setTestSelfLinker{
		t:           t,
		expectedSet: "/api/version/simple/" + ID + "?namespace=default",
		name:        ID,
		namespace:   api.NamespaceDefault,
	}
	handler := handleLinker(storage, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &Simple{
		ObjectMeta: api.ObjectMeta{
			Name:      ID,
			Namespace: "", // update should allow the client to send an empty namespace
		},
		Other: "bar",
	}
	body, err := codec.Encode(item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/"+ID, bytes.NewReader(body))
	_, err = client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if simpleStorage.updated == nil || simpleStorage.updated.Name != item.Name {
		t.Errorf("Unexpected update value %#v, expected %#v.", simpleStorage.updated, item)
	}
	if !selfLinker.called {
		t.Errorf("Never set self link")
	}
}

func TestUpdateInvokesAdmissionControl(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handleDeny(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &Simple{
		ObjectMeta: api.ObjectMeta{
			Name:      ID,
			Namespace: api.NamespaceDefault,
		},
		Other: "bar",
	}
	body, err := codec.Encode(item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/"+ID, bytes.NewReader(body))
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestUpdateRequiresMatchingName(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handleDeny(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &Simple{
		Other: "bar",
	}
	body, err := codec.Encode(item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/"+ID, bytes.NewReader(body))
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestUpdateAllowsMissingNamespace(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &Simple{
		ObjectMeta: api.ObjectMeta{
			Name: ID,
		},
		Other: "bar",
	}
	body, err := codec.Encode(item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/"+ID, bytes.NewReader(body))
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("Unexpected response %#v", response)
	}
}

// when the object name and namespace can't be retrieved, skip name checking
func TestUpdateAllowsMismatchedNamespaceOnError(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	selfLinker := &setTestSelfLinker{
		t:   t,
		err: fmt.Errorf("test error"),
	}
	handler := handleLinker(storage, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &Simple{
		ObjectMeta: api.ObjectMeta{
			Name:      ID,
			Namespace: "other", // does not match request
		},
		Other: "bar",
	}
	body, err := codec.Encode(item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/"+ID, bytes.NewReader(body))
	_, err = client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if simpleStorage.updated == nil || simpleStorage.updated.Name != item.Name {
		t.Errorf("Unexpected update value %#v, expected %#v.", simpleStorage.updated, item)
	}
	if selfLinker.called {
		t.Errorf("self link ignored")
	}
}

func TestUpdatePreventsMismatchedNamespace(t *testing.T) {
	storage := map[string]RESTStorage{}
	simpleStorage := SimpleRESTStorage{}
	ID := "id"
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &Simple{
		ObjectMeta: api.ObjectMeta{
			Name:      ID,
			Namespace: "other",
		},
		Other: "bar",
	}
	body, err := codec.Encode(item)
	if err != nil {
		// The following cases will fail, so die now
		t.Fatalf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/"+ID, bytes.NewReader(body))
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestUpdateMissing(t *testing.T) {
	storage := map[string]RESTStorage{}
	ID := "id"
	simpleStorage := SimpleRESTStorage{
		errors: map[string]error{"update": apierrs.NewNotFound("simple", ID)},
	}
	storage["simple"] = &simpleStorage
	handler := handle(storage)
	server := httptest.NewServer(handler)
	defer server.Close()

	item := &Simple{
		ObjectMeta: api.ObjectMeta{
			Name:      ID,
			Namespace: api.NamespaceDefault,
		},
		Other: "bar",
	}
	body, err := codec.Encode(item)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	client := http.Client{}
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/"+ID, bytes.NewReader(body))
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestCreateNotFound(t *testing.T) {
	handler := handle(map[string]RESTStorage{
		"simple": &SimpleRESTStorage{
			// storage.Create can fail with not found error in theory.
			// See https://github.com/GoogleCloudPlatform/kubernetes/pull/486#discussion_r15037092.
			errors: map[string]error{"create": apierrs.NewNotFound("simple", "id")},
		},
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &Simple{Other: "foo"}
	data, _ := codec.Encode(simple)
	request, err := http.NewRequest("POST", server.URL+"/api/version/simple", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("Unexpected response %#v", response)
	}
}

func TestCreateChecksDecode(t *testing.T) {
	handler := handle(map[string]RESTStorage{"simple": &SimpleRESTStorage{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &api.Pod{}
	data, _ := codec.Encode(simple)
	request, err := http.NewRequest("POST", server.URL+"/api/version/simple", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
	if b, _ := ioutil.ReadAll(response.Body); !strings.Contains(string(b), "must be of type Simple") {
		t.Errorf("unexpected response: %s", string(b))
	}
}

func TestUpdateChecksDecode(t *testing.T) {
	handler := handle(map[string]RESTStorage{"simple": &SimpleRESTStorage{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &api.Pod{}
	data, _ := codec.Encode(simple)
	request, err := http.NewRequest("PUT", server.URL+"/api/version/simple/bar", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("Unexpected response %#v", response)
	}
	if b, _ := ioutil.ReadAll(response.Body); !strings.Contains(string(b), "must be of type Simple") {
		t.Errorf("unexpected response: %s", string(b))
	}
}

func TestParseTimeout(t *testing.T) {
	if d := parseTimeout(""); d != 30*time.Second {
		t.Errorf("blank timeout produces %v", d)
	}
	if d := parseTimeout("not a timeout"); d != 30*time.Second {
		t.Errorf("bad timeout produces %v", d)
	}
	if d := parseTimeout("10s"); d != 10*time.Second {
		t.Errorf("10s timeout produced: %v", d)
	}
}

type setTestSelfLinker struct {
	t           *testing.T
	expectedSet string
	name        string
	namespace   string
	called      bool
	err         error
}

func (s *setTestSelfLinker) Namespace(runtime.Object) (string, error) { return s.namespace, s.err }
func (s *setTestSelfLinker) Name(runtime.Object) (string, error)      { return s.name, s.err }
func (s *setTestSelfLinker) SelfLink(runtime.Object) (string, error)  { return "", s.err }
func (s *setTestSelfLinker) SetSelfLink(obj runtime.Object, selfLink string) error {
	if e, a := s.expectedSet, selfLink; e != a {
		s.t.Errorf("expected '%v', got '%v'", e, a)
	}
	s.called = true
	return s.err
}

func TestCreate(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			time.Sleep(5 * time.Millisecond)
			return obj, nil
		},
	}
	selfLinker := &setTestSelfLinker{
		t:           t,
		name:        "bar",
		namespace:   "default",
		expectedSet: "/api/version/foo/bar?namespace=default",
	}
	handler := handleLinker(map[string]RESTStorage{"foo": &storage}, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &Simple{
		Other: "bar",
	}
	data, _ := codec.Encode(simple)
	request, err := http.NewRequest("POST", server.URL+"/api/version/foo", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	var itemOut Simple
	body, err := extractBody(response, &itemOut)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(&itemOut, simple) {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simple, string(body))
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusOK, response)
	}
	if !selfLinker.called {
		t.Errorf("Never set self link")
	}
}

func TestCreateInNamespace(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			time.Sleep(5 * time.Millisecond)
			return obj, nil
		},
	}
	selfLinker := &setTestSelfLinker{
		t:           t,
		name:        "bar",
		namespace:   "other",
		expectedSet: "/api/version/foo/bar?namespace=other",
	}
	handler := handleLinker(map[string]RESTStorage{"foo": &storage}, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &Simple{
		Other: "bar",
	}
	data, _ := codec.Encode(simple)
	request, err := http.NewRequest("POST", server.URL+"/api/version/foo?namespace=other", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	var itemOut Simple
	body, err := extractBody(response, &itemOut)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(&itemOut, simple) {
		t.Errorf("Unexpected data: %#v, expected %#v (%s)", itemOut, simple, string(body))
	}
	if response.StatusCode != http.StatusCreated {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusOK, response)
	}
	if !selfLinker.called {
		t.Errorf("Never set self link")
	}
}

func TestCreateInvokesAdmissionControl(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			time.Sleep(5 * time.Millisecond)
			return obj, nil
		},
	}
	selfLinker := &setTestSelfLinker{
		t:           t,
		name:        "bar",
		namespace:   "other",
		expectedSet: "/api/version/foo/bar?namespace=other",
	}
	handler := handleInternal(map[string]RESTStorage{"foo": &storage}, deny.NewAlwaysDeny(), mapper, selfLinker)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := http.Client{}

	simple := &Simple{
		Other: "bar",
	}
	data, _ := codec.Encode(simple)
	request, err := http.NewRequest("POST", server.URL+"/api/version/foo?namespace=other", bytes.NewBuffer(data))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	var response *http.Response
	go func() {
		response, err = client.Do(request)
		wg.Done()
	}()
	wg.Wait()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("Unexpected status: %d, Expected: %d, %#v", response.StatusCode, http.StatusForbidden, response)
	}
}

func expectApiStatus(t *testing.T, method, url string, data []byte, code int) *api.Status {
	client := http.Client{}
	request, err := http.NewRequest(method, url, bytes.NewBuffer(data))
	if err != nil {
		t.Fatalf("unexpected error %#v", err)
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("unexpected error on %s %s: %v", method, url, err)
		return nil
	}
	var status api.Status
	_, err = extractBody(response, &status)
	if err != nil {
		t.Fatalf("unexpected error on %s %s: %v", method, url, err)
		return nil
	}
	if code != response.StatusCode {
		t.Fatalf("Expected %s %s to return %d, Got %d", method, url, code, response.StatusCode)
	}
	return &status
}

func TestDelayReturnsError(t *testing.T) {
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			return nil, apierrs.NewAlreadyExists("foo", "bar")
		},
	}
	handler := handle(map[string]RESTStorage{"foo": &storage})
	server := httptest.NewServer(handler)
	defer server.Close()

	status := expectApiStatus(t, "DELETE", fmt.Sprintf("%s/api/version/foo/bar", server.URL), nil, http.StatusConflict)
	if status.Status != api.StatusFailure || status.Message == "" || status.Details == nil || status.Reason != api.StatusReasonAlreadyExists {
		t.Errorf("Unexpected status %#v", status)
	}
}

type UnregisteredAPIObject struct {
	Value string
}

func (*UnregisteredAPIObject) IsAnAPIObject() {}

func TestWriteJSONDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeJSON(http.StatusOK, codec, &UnregisteredAPIObject{"Undecodable"}, w)
	}))
	defer server.Close()
	status := expectApiStatus(t, "GET", server.URL, nil, http.StatusInternalServerError)
	if status.Reason != api.StatusReasonUnknown {
		t.Errorf("unexpected reason %#v", status)
	}
	if !strings.Contains(status.Message, "type apiserver.UnregisteredAPIObject is not registered") {
		t.Errorf("unexpected message %#v", status)
	}
}

type marshalError struct {
	err error
}

func (m *marshalError) MarshalJSON() ([]byte, error) {
	return []byte{}, m.err
}

func TestWriteRAWJSONMarshalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeRawJSON(http.StatusOK, &marshalError{errors.New("Undecodable")}, w)
	}))
	defer server.Close()
	client := http.Client{}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("unexpected status code %d", resp.StatusCode)
	}
}

func TestCreateTimeout(t *testing.T) {
	testOver := make(chan struct{})
	defer close(testOver)
	storage := SimpleRESTStorage{
		injectedFunction: func(obj runtime.Object) (runtime.Object, error) {
			// Eliminate flakes by ensuring the create operation takes longer than this test.
			<-testOver
			return obj, nil
		},
	}
	handler := handle(map[string]RESTStorage{
		"foo": &storage,
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	simple := &Simple{Other: "foo"}
	data, _ := codec.Encode(simple)
	itemOut := expectApiStatus(t, "POST", server.URL+"/api/version/foo?timeout=4ms", data, apierrs.StatusServerTimeout)
	if itemOut.Status != api.StatusFailure || itemOut.Reason != api.StatusReasonTimeout {
		t.Errorf("Unexpected status %#v", itemOut)
	}
}

func TestCORSAllowedOrigins(t *testing.T) {
	table := []struct {
		allowedOrigins util.StringList
		origin         string
		allowed        bool
	}{
		{[]string{}, "example.com", false},
		{[]string{"example.com"}, "example.com", true},
		{[]string{"example.com"}, "not-allowed.com", false},
		{[]string{"not-matching.com", "example.com"}, "example.com", true},
		{[]string{".*"}, "example.com", true},
	}

	for _, item := range table {
		allowedOriginRegexps, err := util.CompileRegexps(item.allowedOrigins)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		handler := CORS(
			handle(map[string]RESTStorage{}),
			allowedOriginRegexps, nil, nil, "true",
		)
		server := httptest.NewServer(handler)
		defer server.Close()
		client := http.Client{}

		request, err := http.NewRequest("GET", server.URL+"/version", nil)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		request.Header.Set("Origin", item.origin)

		response, err := client.Do(request)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if item.allowed {
			if !reflect.DeepEqual(item.origin, response.Header.Get("Access-Control-Allow-Origin")) {
				t.Errorf("Expected %#v, Got %#v", item.origin, response.Header.Get("Access-Control-Allow-Origin"))
			}

			if response.Header.Get("Access-Control-Allow-Credentials") == "" {
				t.Errorf("Expected Access-Control-Allow-Credentials header to be set")
			}

			if response.Header.Get("Access-Control-Allow-Headers") == "" {
				t.Errorf("Expected Access-Control-Allow-Headers header to be set")
			}

			if response.Header.Get("Access-Control-Allow-Methods") == "" {
				t.Errorf("Expected Access-Control-Allow-Methods header to be set")
			}
		} else {
			if response.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("Expected Access-Control-Allow-Origin header to not be set")
			}

			if response.Header.Get("Access-Control-Allow-Credentials") != "" {
				t.Errorf("Expected Access-Control-Allow-Credentials header to not be set")
			}

			if response.Header.Get("Access-Control-Allow-Headers") != "" {
				t.Errorf("Expected Access-Control-Allow-Headers header to not be set")
			}

			if response.Header.Get("Access-Control-Allow-Methods") != "" {
				t.Errorf("Expected Access-Control-Allow-Methods header to not be set")
			}
		}
	}
}
