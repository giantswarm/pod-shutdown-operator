package main

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestParseFluxControllers(t *testing.T) {
	tests := []struct {
		list    string
		want    []types.NamespacedName
		wantErr bool
	}{
		{list: "", want: nil},
		{
			list: "flux-system/kustomize-controller, flux-system/helm-controller",
			want: []types.NamespacedName{
				{Namespace: "flux-system", Name: "kustomize-controller"},
				{Namespace: "flux-system", Name: "helm-controller"},
			},
		},
		{list: "kustomize-controller", wantErr: true},
		{list: "/kustomize-controller", wantErr: true},
		{list: "flux-system/", wantErr: true},
		{list: "flux-system/a/b", wantErr: true},
		{list: "flux-system/kustomize-controller,", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseFluxControllers(tt.list)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseFluxControllers(%q) error = %v, want an error: %v", tt.list, err, tt.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseFluxControllers(%q) = %v, want %v", tt.list, got, tt.want)
		}
	}
}

func TestParseOperatorDeployment(t *testing.T) {
	tests := []struct {
		value   string
		want    types.NamespacedName
		wantErr bool
	}{
		{value: "", want: types.NamespacedName{}},
		{value: "giantswarm/pod-shutdown-operator", want: types.NamespacedName{Namespace: "giantswarm", Name: "pod-shutdown-operator"}},
		{value: "pod-shutdown-operator", wantErr: true},
		{value: "giantswarm/", wantErr: true},
		{value: "giantswarm/a,giantswarm/b", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseOperatorDeployment(tt.value)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseOperatorDeployment(%q) error = %v, want an error: %v", tt.value, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseOperatorDeployment(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}
