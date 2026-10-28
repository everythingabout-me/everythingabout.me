package cos

import "testing"

func TestSTSResource(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{
			key:  "drive/u1/Photos/a.jpg",
			want: "qcs::cos:ap-beijing:uid/1250000000:bucket-1250000000/drive/u1/Photos/a.jpg",
		},
		{
			// Without encoding, STS reads + as a space and the grant would not match the real key
			key:  "drive/u1/运动样例/选手甲+选手乙vsPlayerC+PlayerD.MOV",
			want: "qcs::cos:ap-beijing:uid/1250000000:bucket-1250000000/drive/u1/%E8%BF%90%E5%8A%A8%E6%A0%B7%E4%BE%8B/%E9%80%89%E6%89%8B%E7%94%B2%2B%E9%80%89%E6%89%8B%E4%B9%99vsPlayerC%2BPlayerD.MOV",
		},
		{
			key:  "drive/u1/2004-03-19 样例锦标赛/50%#1.mov",
			want: "qcs::cos:ap-beijing:uid/1250000000:bucket-1250000000/drive/u1/2004-03-19%20%E6%A0%B7%E4%BE%8B%E9%94%A6%E6%A0%87%E8%B5%9B/50%25%231.mov",
		},
	}
	for _, tc := range cases {
		got := stsResource("ap-beijing", "1250000000", "bucket-1250000000", tc.key)
		if got != tc.want {
			t.Fatalf("stsResource(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
