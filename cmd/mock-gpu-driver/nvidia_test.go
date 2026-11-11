package main

import "testing"

func TestParseNvidiaSMIOutput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []nvidiaGPU
		wantErr bool
	}{
		{
			name: "single gpu",
			in:   "0, GPU-1234abcd-0000-0000-0000-000000000000, NVIDIA A100-SXM4-40GB, 40960\n",
			want: []nvidiaGPU{{index: "0", uuid: "GPU-1234abcd-0000-0000-0000-000000000000", model: "NVIDIA A100-SXM4-40GB", memMiB: 40960}},
		},
		{
			name: "multiple gpus, trailing blank line",
			in: "0, GPU-aaa, NVIDIA H100, 81559\n" +
				"1, GPU-bbb, NVIDIA H100, 81559\n\n",
			want: []nvidiaGPU{
				{index: "0", uuid: "GPU-aaa", model: "NVIDIA H100", memMiB: 81559},
				{index: "1", uuid: "GPU-bbb", model: "NVIDIA H100", memMiB: 81559},
			},
		},
		{
			name:    "too few fields",
			in:      "not,enough\n",
			wantErr: true,
		},
		{
			name:    "non-numeric memory",
			in:      "0, GPU-aaa, NVIDIA H100, not-a-number\n",
			wantErr: true,
		},
		{
			name: "empty output",
			in:   "",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNvidiaSMIOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got none (result: %+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d gpus, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("gpu %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
