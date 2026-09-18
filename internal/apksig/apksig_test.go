package apksig

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
)

// 本文件的 fixture 全部是**手搓的合成包**，一个真实 APK 都不需要。
//
// 这一点值得先说清楚，因为它决定了这些测试能证明什么、不能证明什么：
//
//	能证明  结构解析走对了 —— 取的是哪个方案的哪一段字节、边界与损坏输入会不会被挡住、
//	        EOCD 在什么情况下会被认错。合成包是这里**唯一**能把"坏输入"造出来的手段，
//	        而坏输入恰恰是真实 APK 几乎不会提供的。
//	不能证明 我们算出来的指纹与 fdroidserver 算出来的**逐位相同**。那件事只能拿一个
//	        真 APK 同时喂给两边。本机没有 fdroidserver（它只在 debian:trixie 里），
//	        所以那个反馈环**只在 CI 里闭合**：`fdroid update` 会把
//	        `manifest.signer.sha256` 写进 index-v2.json，而 check-repo 会把它与账本里
//	        记的值逐条比对 —— 那个比对就是这个包的端到端验收，跑在每一个真实 APK 上。
//
// 所以：这里的每一条断言都是结构性的，"与 fdroidserver 一致"由 CI 里的交叉校验兜底。

// ---- 造包的工具 -------------------------------------------------------------

// certFixture 是一张**假证书**：它不是一个合法的 X.509（没有名字、没有公钥），
// 而这正是可以的 —— 从签名块里取证书的那段代码不解它的任何字段，只按 TLV 切字节。
//
// 内容凑到 200 字节是为了跨过 DER 短形式的 127 字节上限，从而走到 readDERLen 的
// **长形式**分支：真实证书动辄一千多字节，长形式才是常态，短形式反而是边角。
func certFixture(fill byte) []byte {
	body := bytes.Repeat([]byte{fill}, 200)
	return derTLV(0x30, body)
}

// derTLV 按 DER 拼一段 TLV。
func derTLV(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, derLen(len(content))...)
	return append(out, content...)
}

// derLen 是 readDERLen 的**逆**：短形式能装下就用短形式，否则用长形式。
//
// 它刻意与 readDERLen 互相独立地写（那边是读、这边是写），这样两边都错才有可能
// 一起错 —— 而"两边错成一样"的概率远小于"一处写错"。
func derLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var be []byte
	for v := n; v > 0; v >>= 8 {
		be = append([]byte{byte(v & 0xFF)}, be...)
	}
	return append([]byte{0x80 | byte(len(be))}, be...)
}

// u32 是长度前缀（小端）拼接。
func u32(n int) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(n))
	return b[:]
}

// prefixed 是"长度 + 内容"。
func prefixed(b []byte) []byte { return append(u32(len(b)), b...) }

// schemeValue 造一个 v2/v3 签名块的值：一个签名者、一张证书。布局见 firstSignerCert 的注释。
//
// ⚠️ `size_signer` 这个长度**含它自己那 4 个字节**（androguard 是拿它去切
// `view[off_signer : off_signer+size_signer]` 的）。所以它是 `u32(len(signer)+4)` 而
// 不是 `u32(len(signer))` —— 而 `size_sequence` 又**不含**它自己（它 + 4 才是整段）。
// 两个长度一个含头一个不含，这是这一段里唯一容易写反的地方，写反了会往值里多塞一层
// 长度，而解析器会安稳地把它当成真实数据读下去。
func schemeValue(certs ...[]byte) []byte {
	var certBlob []byte
	for _, c := range certs {
		certBlob = append(certBlob, prefixed(c)...)
	}

	signed := concat(
		prefixed([]byte{1, 2, 3, 4}), // digests：内容无所谓，我们不验签
		prefixed(certBlob),           // certificates
		prefixed(nil),                // additional attributes
	)

	signer := concat(
		u32(len(signed)+4),     // size_signer（含这 4 字节自己）
		prefixed(signed),       // signed data
		prefixed([]byte{9, 9}), // signatures
		prefixed([]byte{7}),    // public key
	)

	return concat(u32(len(signer)), signer) // size_sequence（不含这 4 字节自己）+ 签名者
}

// OID 1.2.840.113549.1.7.2（signedData）。
var oidSignedData = []byte{0x06, 0x09, 0x2A, 0x86, 0x48, 0x86, 0xF7, 0x0D, 0x01, 0x07, 0x02}

// pkcs7Fixture 造一份最小但结构完整的 PKCS#7 ContentInfo(SignedData)。
//
// 结构按 RFC 5652 摆全（version / digestAlgorithms / encapContentInfo / certificates /
// signerInfos），而不是只留下解析器要的那一段 —— 因为解析器**不看这些字段**、
// 只在直接子元素里找 [0]，所以少摆几个字段它照样能过。摆全才测得出"它真的没数错位置"。
func pkcs7Fixture(certs ...[]byte) []byte {
	var carr []byte
	for _, c := range certs {
		carr = append(carr, c...)
	}

	sd := derTLV(0x30, concat(
		derTLV(0x02, []byte{1}), // version
		derTLV(0x31, nil),       // digestAlgorithms
		derTLV(0x30, nil),       // encapContentInfo
		derTLV(0xA0, carr),      // certificates [0] IMPLICIT
		derTLV(0x31, nil),       // signerInfos
	))
	return derTLV(0x30, concat(oidSignedData, derTLV(0xA0, sd)))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// buildZip 用 archive/zip 写一个最小归档。
func buildZip(t *testing.T, files map[string][]byte, comment string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// 名字排一遍序，免得 map 的随机顺序让产物每跑一次都不一样（测试不该依赖顺序）。
	for _, name := range sortedKeys(files) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("写 %s：%v", name, err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatalf("写 %s 的内容：%v", name, err)
		}
	}
	if comment != "" {
		if err := zw.SetComment(comment); err != nil {
			t.Fatalf("设注释：%v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关 zip：%v", err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// spliceSigningBlock 把签名块插到中央目录之前 —— 也就是它在真实 APK 里的位置。
//
// 中央目录的偏移**直接从 EOCD 里读**（第 16 字节那个 uint32），然后用
// `cdOff + cdSize == eocd` 这条恒等式自证没读错。这个工具因此与 centralDirOffset
// 的实现**没有共同逻辑** —— 拿同一套扫描逻辑去造 fixture，等于让测试与实际实现一起错。
func spliceSigningBlock(t *testing.T, z []byte, pairs ...blockPair) []byte {
	t.Helper()
	eocd := len(z) - 22 - commentLen(z)
	if eocd < 0 || !bytes.Equal(z[eocd:eocd+4], []byte{'P', 'K', 5, 6}) {
		t.Fatalf("EOCD 不在预期的位置 %d 上", eocd)
	}
	cdOff := int(binary.LittleEndian.Uint32(z[eocd+16 : eocd+20]))
	cdSize := int(binary.LittleEndian.Uint32(z[eocd+12 : eocd+16]))
	if cdOff+cdSize != eocd {
		t.Fatalf("中央目录的偏移/长度不成立：%d + %d != %d", cdOff, cdSize, eocd)
	}

	block := sigBlock(pairs)
	out := concat(z[:cdOff], block, z[cdOff:])

	// EOCD 里的偏移要跟着挪，否则 zip 库会找不到中央目录。
	shift := len(block)
	binary.LittleEndian.PutUint32(out[eocd+shift+16:], uint32(cdOff+shift))
	return out
}

// commentLen 从 EOCD 里读出注释长度。EOCD 定长 22 字节、注释紧紧跟在它后面直到文件
// 末尾，所以 EOCD 的位置是 `len - 22 - 注释长度` —— 这也正是这里要拿它算的东西。
func commentLen(z []byte) int {
	if len(z) < 22 {
		return 0
	}
	return int(binary.LittleEndian.Uint16(z[len(z)-22+20 : len(z)-22+22]))
}

type blockPair struct {
	id  uint32
	val []byte
}

// sigBlock 按 AOSP 的格式拼出整个签名块（含首尾两个长度字段与末尾的魔数）。
//
// ⚠️ 每一对前面那个长度是 **uint64**，不是 uint32 —— 块里只有 ID、值内部的长度才是
// uint32。这一处不对称很容易写反，而写反的症状是解析器读到一段完全错位的字节。
//
// 两个块长度字段的值相同，都是"除开头那 8 字节之外的全部" —— 这也是生产代码用来
// 反推块起点的那条等式（`start = cdOff - 8 - blockSize`）。
func sigBlock(pairs []blockPair) []byte {
	var body []byte
	for _, p := range pairs {
		v := make([]byte, 4, 4+len(p.val))
		binary.LittleEndian.PutUint32(v, p.id)
		v = append(v, p.val...)

		lenU64 := make([]byte, 8)
		binary.LittleEndian.PutUint64(lenU64, uint64(len(v)))
		body = append(body, lenU64...)
		body = append(body, v...)
	}

	size := make([]byte, 8)
	binary.LittleEndian.PutUint64(size, uint64(len(body)+8+16))
	return concat(size, body, size, []byte(sigBlockMagic))
}

// apk 造一个完整的合成 APK：一个占位条目 + 可选的签名块 + 可选的其他条目。
func apk(t *testing.T, pairs []blockPair, extra map[string][]byte) []byte {
	t.Helper()
	files := map[string][]byte{"classes.dex": []byte("not really a dex")}
	for k, v := range extra {
		files[k] = v
	}
	z := buildZip(t, files, "")
	if len(pairs) == 0 {
		return z
	}
	out := spliceSigningBlock(t, z, pairs...)

	// 独立校验：拼完之后这个归档还必须能被 zip 库正常打开，而且每个条目都还在。
	// 这条断言是"插入点找对了"的唯一证据 —— 插错位置会把中央目录劈开，那之后
	// 读得出来才怪。它同时兜住了 spliceSigningBlock 里那段偏移推算。
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("插入签名块之后归档读不开了：%v", err)
	}
	for _, f := range zr.File {
		if _, ok := files[f.Name]; !ok {
			t.Fatalf("插入签名块之后多出一个条目 %q", f.Name)
		}
		delete(files, f.Name)
	}
	if len(files) != 0 {
		t.Fatalf("插入签名块之后丢了 %d 个条目", len(files))
	}
	return out
}

func readAPK(t *testing.T, b []byte) (*Signer, error) {
	t.Helper()
	return ReadZip(bytes.NewReader(b), int64(len(b)))
}

func wantSHA(cert []byte) string {
	sum := sha256.Sum256(cert)
	return hex.EncodeToString(sum[:])
}

// ---- 签名块的方案选择 --------------------------------------------------------

func TestReadPrefersV3(t *testing.T) {
	cert := certFixture(0xAA)
	certV2 := certFixture(0xAA) // 同一把钥匙 ⇒ 同一张证书 ⇒ 两个方案的字节相同
	b := apk(t, []blockPair{
		{v3BlockID, schemeValue(cert)},
		{v2BlockID, schemeValue(certV2)},
	}, nil)

	s, err := readAPK(t, b)
	if err != nil {
		t.Fatalf("读签名者：%v", err)
	}
	if s.Scheme != "v3" {
		t.Errorf("方案 = %q，想要 v3 —— 规则 1 说 v3 优先，不是 v2", s.Scheme)
	}
	if s.SHA256 != wantSHA(cert) {
		t.Errorf("指纹 = %s，想要 %s", s.SHA256, wantSHA(cert))
	}
	if len(s.SHA256) != 64 {
		t.Errorf("指纹长度 = %d，想要 64（小写十六进制的 sha256，不带分隔符）", len(s.SHA256))
	}
}

func TestReadFallsBackToV2(t *testing.T) {
	cert := certFixture(0xBB)
	b := apk(t, []blockPair{{v2BlockID, schemeValue(cert)}}, nil)

	s, err := readAPK(t, b)
	if err != nil {
		t.Fatalf("读签名者：%v", err)
	}
	if s.Scheme != "v2" {
		t.Errorf("方案 = %q，想要 v2", s.Scheme)
	}
	if s.SHA256 != wantSHA(cert) {
		t.Errorf("指纹 = %s，想要 %s", s.SHA256, wantSHA(cert))
	}
}

// 规则 5：两个方案的证书不同 = 这个包自己就签坏了。fdroidserver 会返回 None，
// 我们报错 —— 两边都拒绝它，差别只是"拒绝"这件事有没有人说出口。
func TestReadRejectsMismatchedSchemes(t *testing.T) {
	b := apk(t, []blockPair{
		{v3BlockID, schemeValue(certFixture(0xAA))},
		{v2BlockID, schemeValue(certFixture(0xBB))},
	}, nil)

	_, err := readAPK(t, b)
	if err == nil {
		t.Fatal("两个方案的证书不同却没有报错")
	}
	if !strings.Contains(err.Error(), "签坏了") {
		t.Errorf("错误信息没说到点子上：%v", err)
	}
}

// v1 只在 v2/v3 都不存在时才被读到（规则 4）。两者都在时 v1 根本不进候选集，
// 所以即使它带着一张不同的证书也不该影响结果。
func TestReadIgnoresV1WhenV2Present(t *testing.T) {
	cert := certFixture(0xCC)
	other := certFixture(0xDD)
	b := apk(t, []blockPair{{v2BlockID, schemeValue(cert)}}, map[string][]byte{
		"META-INF/CERT.RSA": pkcs7Fixture(other),
	})

	s, err := readAPK(t, b)
	if err != nil {
		t.Fatalf("读签名者：%v", err)
	}
	if s.Scheme != "v2" || s.SHA256 != wantSHA(cert) {
		t.Errorf("得到 %s/%s，想要 v2/%s", s.Scheme, s.SHA256, wantSHA(cert))
	}
}

// ---- v1（JAR 签名） ----------------------------------------------------------

func TestReadV1(t *testing.T) {
	cert := certFixture(0xEE)
	b := apk(t, nil, map[string][]byte{"META-INF/CERT.RSA": pkcs7Fixture(cert)})

	s, err := readAPK(t, b)
	if err != nil {
		t.Fatalf("读签名者：%v", err)
	}
	if s.Scheme != "v1" {
		t.Errorf("方案 = %q，想要 v1", s.Scheme)
	}
	if s.SHA256 != wantSHA(cert) {
		t.Errorf("指纹 = %s，想要 %s", s.SHA256, wantSHA(cert))
	}
}

// 三种后缀都认，而且**子目录也认** —— 判据是 fdroidserver 那个带 `.*` 的正则。
func TestV1BlockNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"META-INF/CERT.RSA", true},
		{"META-INF/CERT.DSA", true},
		{"META-INF/CERT.EC", true},
		{"META-INF/SIG-FOO/FOO.RSA", true}, // 正则里 `.*` 允许子目录
		{"META-INF/.RSA", true},            // `.*` 可以配空串
		{"META-INF/CERT.SF", false},        // .SF 是签名文件，不是签名块文件
		{"META-INF/MANIFEST.MF", false},
		{"META-INF/CERT.rsa", false}, // 正则是大小写敏感的
		{"assets/CERT.RSA", false},
		{"CERT.RSA", false},
		{"META-INF", false},
	} {
		if got := isV1BlockName(tc.name); got != tc.want {
			t.Errorf("isV1BlockName(%q) = %v，想要 %v", tc.name, got, tc.want)
		}
	}
}

// 两个签名块文件 = 这个包被签了不止一次。fdroidserver 直接判错，我们也判错 ——
// 挑第一个的后果是把某个已经作废的签名者的指纹记下来。
func TestReadRejectsMultipleV1Blocks(t *testing.T) {
	b := apk(t, nil, map[string][]byte{
		"META-INF/CERT.RSA":  pkcs7Fixture(certFixture(0x11)),
		"META-INF/OTHER.DSA": pkcs7Fixture(certFixture(0x22)),
	})

	_, err := readAPK(t, b)
	if err == nil {
		t.Fatal("两个签名块文件却没有报错")
	}
	if !strings.Contains(err.Error(), "2 个") {
		t.Errorf("错误信息没报出数量：%v", err)
	}
}

// 证书链：fdroidserver 会验签挑出真正签名的那一张，我们不验 —— 所以报错而不是猜。
func TestReadRejectsV1CertChain(t *testing.T) {
	b := apk(t, nil, map[string][]byte{
		"META-INF/CERT.RSA": pkcs7Fixture(certFixture(0x11), certFixture(0x22)),
	})

	_, err := readAPK(t, b)
	if err == nil {
		t.Fatal("证书链却没有报错")
	}
	if !strings.Contains(err.Error(), "证书链") {
		t.Errorf("错误信息没说到点子上：%v", err)
	}
}

// ---- 没有签名 / 结构损坏 ------------------------------------------------------

func TestReadNoSignature(t *testing.T) {
	_, err := readAPK(t, apk(t, nil, nil))
	if err == nil {
		t.Fatal("一个没签名的包却没有报错")
	}
	if !strings.Contains(err.Error(), "没有签名证书") {
		t.Errorf("错误信息没说到点子上：%v", err)
	}
}

// 签名块在、但里面一个签名者都没有 —— 这与 androguard 返回空列表是同一件事，
// 应当按"没签名"处理，而不是当成损坏。
func TestReadEmptySignerList(t *testing.T) {
	empty := u32(0) // size_sequence = 0，一个签名者都没有
	_, err := readAPK(t, apk(t, []blockPair{{v2BlockID, empty}}, nil))
	if err == nil || !strings.Contains(err.Error(), "没有签名证书") {
		t.Errorf("想要「没有签名证书」，得到 %v", err)
	}
}

// 值的长度自相矛盾时不能硬着头皮往下读 —— 那会拿一段无关字节算出指纹。
func TestReadRejectsInconsistentLength(t *testing.T) {
	v := schemeValue(certFixture(0x33))
	v[0]++ // 把 size_sequence 改大 1，首尾就对不上了
	_, err := readAPK(t, apk(t, []blockPair{{v2BlockID, v}}, nil))
	if err == nil {
		t.Fatal("长度自相矛盾却没有报错")
	}
	if !strings.Contains(err.Error(), "长度自相矛盾") {
		t.Errorf("错误信息没说到点子上：%v", err)
	}
}

// 中央目录之前那段字节不是签名块（魔数不对）时，应当当成"没有块"而不是报错 ——
// 中央目录之前本来就允许有任意数据。
func TestNoSigningBlockTreatedAsAbsent(t *testing.T) {
	cert := certFixture(0x44)
	z := buildZip(t, map[string][]byte{"META-INF/CERT.RSA": pkcs7Fixture(cert)}, "")
	// 在中央目录之前插 64 个字节的垃圾（不是签名块）。
	junk := bytes.Repeat([]byte{0x5A}, 64)
	eocd := len(z) - 22
	cdOff := int(binary.LittleEndian.Uint32(z[eocd+16 : eocd+20]))

	out := concat(z[:cdOff], junk, z[cdOff:])
	newEOCD := eocd + len(junk)
	binary.LittleEndian.PutUint32(out[newEOCD+16:], uint32(cdOff+len(junk)))

	// 垃圾在中央目录之前 ⇒ 没有签名块 ⇒ 回落到 v1，并且读得出来。
	s, err := ReadZip(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("读签名者：%v", err)
	}
	if s.Scheme != "v1" || s.SHA256 != wantSHA(cert) {
		t.Errorf("得到 %s/%s，想要 v1/%s", s.Scheme, s.SHA256, wantSHA(cert))
	}
}

// ZIP 注释里可以合法地含有 `PK\x05\x06` 这四个字节。从后往前扫时**必须**用
// "注释长度 + 22 + 位置 == 文件长度"这条等式把假记录滤掉，否则中央目录的偏移会指向
// 一段毫无关系的位置，然后一切都在错误的偏移上继续（签名块会被"找不到"，
// 或者更糟 —— 在错误的偏移上找到一段恰好长得像签名块的字节）。
//
// 这里只测 centralDirOffset 本身，**不走 ReadZip**：那样走的最后一步是 `zip.NewReader`，
// 而标准库那个实现用的是宽松判据（`n + 22 + i <= len(b)`，见 archive/zip 的
// findSignatureInBlock），碰到这种注释会直接返回 -1 并报 "not a valid zip file"。
// 换句话说，这个包比标准库**更严** —— 而"更严"在这里是唯一正确的方向：
// 中央目录的位置没有第二种解释。代价是这类归档在流水线的其他地方（apkmeta）读不开，
// 但那与签名无关，且那种包本身就进不来。
func TestEOCDScanSkipsFakeSignatureInComment(t *testing.T) {
	comment := "note: PK\x05\x06 is the EOCD signature"
	z := buildZip(t, map[string][]byte{"classes.dex": []byte("x")}, comment)

	real := len(z) - 22 - len(comment)
	want := int64(binary.LittleEndian.Uint32(z[real+16 : real+20]))

	got, err := centralDirOffset(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		t.Fatalf("找中央目录：%v", err)
	}
	if got != want {
		t.Errorf("中央目录偏移 = %d，想要 %d —— 命中注释里那条假记录了", got, want)
	}
}

// ZIP64 时中央目录的偏移是哨兵值 0xFFFFFFFF，真正的值在另一张记录里。
// 不猜 —— 猜出来的结果是一段随机字节被当成签名块。
func TestZIP64Rejected(t *testing.T) {
	z := buildZip(t, map[string][]byte{"classes.dex": []byte("x")}, "")
	eocd := len(z) - 22
	binary.LittleEndian.PutUint32(z[eocd+16:], 0xFFFFFFFF)

	_, err := ReadZip(bytes.NewReader(z), int64(len(z)))
	if err == nil || !strings.Contains(err.Error(), "ZIP64") {
		t.Errorf("想要 ZIP64 的报错，得到 %v", err)
	}
}

// 太小的输入不该走到越界读。
func TestTooSmall(t *testing.T) {
	for _, n := range []int{0, 1, 21} {
		if _, err := ReadZip(bytes.NewReader(make([]byte, n)), int64(n)); err == nil {
			t.Errorf("%d 字节的输入没有报错", n)
		}
	}
}

// ---- DER 遍历器的边界 --------------------------------------------------------

func TestReadDERLen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      []byte
		want    int
		wantN   int
		wantErr bool
	}{
		{"短形式", []byte{0x7F}, 127, 1, false},
		{"短形式零", []byte{0x00}, 0, 1, false},
		{"长形式一字节", []byte{0x81, 0xC8}, 200, 2, false},
		{"长形式两字节", []byte{0x82, 0x01, 0x00}, 256, 3, false},
		{"不定长被拒", []byte{0x80}, 0, 0, true},
		{"超长被拒", []byte{0x85, 1, 2, 3, 4, 5}, 0, 0, true},
		{"长度字段被截断", []byte{0x82, 0x01}, 0, 0, true},
		{"空", nil, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n, err := readDERLen(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("想要报错，得到 %d/%d", got, n)
				}
				return
			}
			if err != nil {
				t.Fatalf("不要报错，得到 %v", err)
			}
			if got != tc.want || n != tc.wantN {
				t.Errorf("得到 %d/%d，想要 %d/%d", got, n, tc.want, tc.wantN)
			}
		})
	}
}

func TestReadTLV(t *testing.T) {
	// 长形式的证书（我们的 fixture 就是长形式）走一圈：full 必须是**含头部**的那一段。
	cert := certFixture(0x66)
	got, n, err := readTLV(cert)
	if err != nil {
		t.Fatalf("读 TLV：%v", err)
	}
	if n != len(cert) {
		t.Errorf("consumed = %d，想要 %d", n, len(cert))
	}
	if !bytes.Equal(got.full, cert) {
		t.Error("full 与输入不一致 —— 证书 DER 要的是含头部的整段")
	}
	if len(got.content) != 200 {
		t.Errorf("content 长度 = %d，想要 200", len(got.content))
	}

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"只有一个字节", []byte{0x30}},
		{"内容被截断", []byte{0x30, 0x05, 1, 2}},
		{"高位标签号", []byte{0x3F, 0x01, 0x00}},
		{"不定长", []byte{0x30, 0x80, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := readTLV(tc.in); err == nil {
				t.Error("想要报错")
			}
		})
	}
}

// 假的 PKCS#7：把 [0] 那一层改成 EXPLICIT（多套一层 SEQUENCE）也必须能读对 ——
// 这是为了钉死"我们取的是 IMPLICIT 的语义"。反过来，如果代码把 IMPLICIT 当 EXPLICIT
// 处理，这个用例会拿到套着的那一层 SEQUENCE 当证书，指纹就对不上了。
func TestPKCS7FirstCert(t *testing.T) {
	cert := certFixture(0x77)
	got, err := pkcs7FirstCert(pkcs7Fixture(cert))
	if err != nil {
		t.Fatalf("取证书：%v", err)
	}
	if !bytes.Equal(got, cert) {
		t.Errorf("取到的不是那张证书（%d 字节 vs %d 字节）", len(got), len(cert))
	}

	// 结构坏掉时一律报错，不返回一段猜出来的字节。
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"不是 SEQUENCE", derTLV(0x31, nil)},
		{"第一个子元素不是 OID", derTLV(0x30, derTLV(0x02, []byte{1}))},
		{"没有 content", derTLV(0x30, oidSignedData)},
		{"没有证书", derTLV(0x30, concat(oidSignedData, derTLV(0xA0, derTLV(0x30, nil))))},
		{"空", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pkcs7FirstCert(tc.in); err == nil {
				t.Error("想要报错")
			}
		})
	}
}
