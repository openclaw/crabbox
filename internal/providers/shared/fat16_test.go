package shared

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBuildFAT16ImageGolden(t *testing.T) {
	files255 := make([]FATFile, 255)
	for i := range files255 {
		files255[i] = FATFile{Name: fmt.Sprintf("f%03d", i)}
	}
	cases := []struct {
		name, label string
		files       []FATFile
		hashes      [2]string
	}{
		{"ordered-files", "cidata", []FATFile{{Name: "user-data", Data: []byte("#cloud-config\nusers:\n- name: alice\n")}, {Name: "meta-data", Data: []byte("instance-id: crabbox-test\nlocal-hostname: my-app\n")}}, [2]string{"8cf6c03a8acdb3642216daadda7a475df2f93d14a90173cdb97a263b121af03d", "a752d9264be7b8d65d7aa76c88b29390db32c01aeb32583018ec5046df8ea2b6"}},
		{"empty-allocation", "cidata", []FATFile{{Name: "empty"}}, [2]string{"f900e21494f8059f3d529bd9488be5f26efffc8bba739a4d2cdf8633fd1722fa", "c58cb3d26116f066a07255508232d7fdbfbfbfe48dd082227d0bfbcb7cf28672"}},
		{"multi-cluster", "cidata", []FATFile{{Name: "payload.bin", Data: bytes.Repeat([]byte("0123456789abcdef"), 321)}}, [2]string{"3965f5a85647fb1900551fed056e49d4a29aad5db4af54967006e66e1a2a80a7", "fd061834ae60dbd1d91eb2be53ed19f51d35866bc4d22c74d7d9a4e593c167e9"}},
		{"label-pad", "id", []FATFile{{Name: "file", Data: []byte("x")}}, [2]string{"07274e4d21666867280db930c9a18536f7ec6a9081a25442c96f31fb3d390001", "9a541636761b52594893419fab8e06ccf03bfdad2385a07764aefaa61abd7b7d"}},
		{"label-truncate", "abcdefghijklmnop", []FATFile{{Name: "file", Data: []byte("x")}}, [2]string{"491f6aec8706725bdba81c2e7cf30e6f2123ad988b1469bf4de92f2c05b233a4", "c41a9dcda7e219bcf48d7f5a289e159bad7505dfea1cea54ad9140382ba0860a"}},
		{"lfn-13-runes", "cidata", []FATFile{{Name: "abcdefghijklm", Data: []byte("x")}}, [2]string{"68246b5f6dcf2f4fafef9eb5895b3b9819831094d0f3d01505c9a0437db55fc4", "ffa3cc58a18d26f5850137eb3010f634541dfcf4e8aa53c6853936e3ad95b57a"}},
		{"lfn-14-runes", "cidata", []FATFile{{Name: "abcdefghijklmn", Data: []byte("x")}}, [2]string{"704d0dfab25c0c1957008c46e720299c40807fc4b336150adfdc9106a0f83eef", "49c0df807932004e021ad11e58fd16be5b5f648bbac9299caa05f0bac0b647d0"}},
		{"lfn-non-bmp", "cidata", []FATFile{{Name: "prefix-\U0001F680-suffix", Data: []byte("x")}}, [2]string{"fe323f8b6492a81375c2129d21df8e7e9c73098e6e6cd355bf305751ae629df9", "b6ef0942918d043c63f14861ce25fca5123f4404b9dbd1769e1c0c718f8df384"}},
		{"directory-255-files", "cidata", files255, [2]string{"e24197abfd70a44bf9b2dcc3b69d87a988441929ca6fef023875ba725294f37f", "8b20aa081aa8a3f9bdf7d2feeef31ce621532f197d8f02c9bfe97cd7af37aeb8"}},
	}
	variants := []struct{ name, short, prefix string }{
		{"firecracker", "FC%06dTXT", "firecracker cloud-init"},
		{"xcpng", "CRAB%04dTXT", "config-drive"},
	}
	for i, variant := range variants {
		for _, test := range cases {
			t.Run(variant.name+"/"+test.name, func(t *testing.T) {
				image, err := BuildFAT16Image(test.label, test.files, variant.short, variant.prefix)
				if err != nil {
					t.Fatal(err)
				}
				if got := fmt.Sprintf("%x", sha256.Sum256(image)); got != test.hashes[i] {
					t.Fatalf("sha256=%s, want %s", got, test.hashes[i])
				}
			})
		}
	}
}

func TestBuildFAT16ImageErrors(t *testing.T) {
	const maxDataBytes = ((20480 - (1 + 2*40 + 512*32/512)) / 4) * 4 * 512
	oversized := bytes.Repeat([]byte{'x'}, maxDataBytes+1)
	files256 := make([]FATFile, 256)
	for i := range files256 {
		files256[i] = FATFile{Name: fmt.Sprintf("f%03d", i)}
	}
	cases := []struct {
		name, suffix string
		files        []FATFile
	}{
		{"blank-name-order", "file name is required", []FATFile{{Name: " ", Data: oversized}}},
		{"capacity-plus-one", "payload is too large", []FATFile{{Name: "payload.bin", Data: oversized}}},
		{"directory-256-files", "directory is too large", files256},
	}
	for _, variant := range []struct{ name, short, prefix string }{{"firecracker", "FC%06dTXT", "firecracker cloud-init"}, {"xcpng", "CRAB%04dTXT", "config-drive"}} {
		for _, test := range cases {
			t.Run(variant.name+"/"+test.name, func(t *testing.T) {
				image, err := BuildFAT16Image("cidata", test.files, variant.short, variant.prefix)
				var exitErr core.ExitError
				if image != nil || !core.AsExitError(err, &exitErr) || exitErr.Code != 2 || exitErr.Message != variant.prefix+" "+test.suffix {
					t.Fatalf("imageNil=%v err=%#v", image == nil, err)
				}
			})
		}
	}
}
