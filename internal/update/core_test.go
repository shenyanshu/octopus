package update

import "testing"

func TestDownloadFilenameFor(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		arch    string
		want    string
		wantErr bool
	}{
		{"linux amd64", "linux", "amd64", "octopus-linux-amd64.zip", false},
		{"linux arm64", "linux", "arm64", "octopus-linux-arm64.zip", false},
		{"windows amd64", "windows", "amd64", "octopus-windows-amd64.zip", false},
		{"windows arm64", "windows", "arm64", "octopus-windows-arm64.zip", false},
		{"darwin amd64", "darwin", "amd64", "octopus-darwin-amd64.zip", false},
		{"darwin arm64", "darwin", "arm64", "octopus-darwin-arm64.zip", false},
		// 已删除的矩阵目标必须报错，防止用户下载到不存在的归档
		{"linux 386 removed", "linux", "386", "", true},
		{"linux arm removed", "linux", "arm", "", true},
		{"android amd64 removed", "android", "amd64", "", true},
		{"android arm64 removed", "android", "arm64", "", true},
		{"freebsd unsupported", "freebsd", "amd64", "", true},
		{"empty", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := downloadFilenameFor(tt.goos, tt.arch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("downloadFilenameFor(%q, %q) error = %v, wantErr %v", tt.goos, tt.arch, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("downloadFilenameFor(%q, %q) = %q, want %q", tt.goos, tt.arch, got, tt.want)
			}
		})
	}
}
