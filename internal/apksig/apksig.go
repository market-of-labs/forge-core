// Package apksig 读出 APK 里**上游开发者**的签名证书指纹。
//
// 这个值就是 fdroidserver 写进 `index-v2.json` 的 `manifest.signer.sha256` 那个字符串，
// 也是客户端拿来做"已装版本能不能升级"判断的依据（02 §2.7）。镜像的时候先把它算出来
// 记进账本，有两个好处：一是不必等 `fdroid update` 之后再回头去索引里找，
// 二是**能在下载当场就拒绝一个没签名的包**（这条决定：只要上游开发者签名的 APK）。
//
// # 它必须与 fdroidserver 逐位一致
//
// 这不是"我们自己定一个指纹格式"的地方。同一个 APK，这里算出来的值必须与 fdroidserver
// 算出来的**完全相同**，否则 check-repo 那条交叉校验会把每一个包都判成错的。所以下面
// 每一条规则都是照着 fdroidserver 的源码抄的（`common.py` 的 `get_first_signer_certificate`
// 与 `signer_fingerprint`），连同它的怪癖一起：
//
//  1. **方案顺序是 v3 → v2 → v1**，不是"v2 优先"。v3 存在就用 v3。
//  2. 取的是**每个方案里第一个签名者的第一张证书**（`certs_v3[0]` / `certs_v2[0]`）。
//  3. 指纹 = **证书 DER 的 SHA-256**，小写十六进制、不带分隔符
//     （`signer_fingerprint` = `hashlib.sha256(cert_encoded).hexdigest()`）——
//     不是对 APK 整体算，也不是对公钥算。
//  4. v1（JAR 签名）只在 `minSdkVersion < 24`、或 v2/v3 都不存在且 targetSdk < 30 时才读。
//  5. 读到的多个方案的证书**必须相等**，否则 fdroidserver 判为"取不到签名者"。
//
// # 与它两处**已知且有意的**偏差
//
// 两处都朝"响亮地失败，而不是静默给一个错值"的方向：
//
//	a. **v1 的读取条件放宽了**：v2/v3 都不存在时一律去读 v1，不看 SDK 版本。
//	   fdroidserver 在"没有 v2/v3 且 targetSdk ≥ 30"时会跳过 v1 而返回 None ——
//	   而那样的 APK 在 Android 11+ 上根本装不上（targetSdk ≥ 30 强制要求 v2+ 签名），
//	   属于病态输入。放宽的后果是"我们说它有签名者、fdroidserver 说没有"，方向是安全的：
//	   索引那边会缺 signer，check-repo 的硬错误会把它挡住（02 §2.8 第 6 条）。
//
//	b. **不做 v1 的签名验证**：一个 PKCS#7 里带多张证书（证书链）时，fdroidserver 会去
//	   验证签名、挑出真正签名的那一张（`get_jar_signer_certificate`，要 oscrypto）。
//	   我们不验证，遇到就直接报错，交给人判断。fdroidserver 自己的注释说这在它的用例里
//	   "should always be a single signer"，属于罕见输入；而猜错的代价是把一张**中间证书**
//	   的指纹写进账本 —— 那是一个看起来完全正常的错值，没有任何一层会为此报错。
//
// 第 5 条那条一致性检查我们**保留**（在放宽后的 v1 读取条件下），因为不相等意味着这个
// APK 自己就签坏了 —— apksigner 会拒绝它（`ApkVerifier` 对所有方案的签名者做一致性
// 检查）。正常的包永远碰不到这条。
package apksig

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// APK Signing Block 的常量。
//
// 三个值都取自 androguard（`androguard/core/apk/__init__.py` 里 APK 类的第 301-303 行），
// 也就是 fdroidserver 实际用的那个解析器 —— 而不是 AOSP 的文档。两者一致，但"与
// 实际跑的那份代码一致"是这里唯一有意义的判据。
const (
	// sigBlockMagic 是签名块末尾那 16 个字节。它同时是"这里有没有签名块"的判据 ——
	// ZIP 中央目录之前那 16 个字节不等它，就说明没有块（只有 v1 签名的包就是这样）。
	sigBlockMagic = "APK Sig Block 42"
	// v2BlockID / v3BlockID 是块里的两种 pair ID。
	v2BlockID = 0x7109871A
	v3BlockID = 0xF05368C0
)

// v23Schemes 是签名块里两种方案的**优先级顺序**。顺序本身就是契约（规则 1），
// 所以写成一条有序的表而不是两个独立的 if —— 将来真要加 v3.1（0x1B93AD61）时，
// 插在哪一位是唯一需要想清楚的事。
var v23Schemes = []struct {
	name string
	id   uint32
}{
	{"v3", v3BlockID},
	{"v2", v2BlockID},
}

// Signer 是一个 APK 的签名者。
type Signer struct {
	// SHA256 是证书 DER 的 SHA-256，小写十六进制、无分隔符 —— 就是索引里那个值。
	SHA256 string
	// Scheme 是取到它的签名方案："v3" / "v2" / "v1"。
	//
	// **它不进索引、也不进账本**：fdroidserver 只记指纹，方案是它的内部日志
	// （`Using APK Signature v3`）。留在这里是因为排查时"这个值是从哪来的"几乎是
	// 第一个要问的问题，而答案是方案 —— 事后重算一遍拿不到它。
	Scheme string
}

// schemeCert 是一次"方案 → 证书"的匹配结果，只为下面那条一致性检查存在。
type schemeCert struct {
	scheme string
	cert   []byte
}

// Read 打开一个 APK 文件并读出它的签名者。
func Read(path string) (*Signer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	s, err := ReadZip(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("apksig: %s：%w", filepath.Base(path), err)
	}
	return s, nil
}

// ReadZip 从**已经打开的**归档里读签名者。
//
// 提供它而不是只给 Read，是为了让调用方能在"APK 已经在手上"的场合复用同一个文件句柄
// （与 apkmeta.ReadZip 同一个理由）—— 而那里正是它要被调用的地方：解析 manifest 与
// 读签名者对着同一个包做，开两次文件是没必要的。
//
// 返回的 error 一律是"这个包不可用"级别的：没签名、签坏了、或者结构读不懂。
// 调用方应当把它当成**拒绝这个包**的判据，而不是"再试一次"。
func ReadZip(r io.ReaderAt, size int64) (*Signer, error) {
	pairs, err := signingBlockPairs(r, size)
	if err != nil {
		return nil, err
	}

	// 先按 v3 → v2 收集。**两个都读出来**（而不是读到第一个就走人）是为了能做规则 5
	// 那条一致性检查 —— 代价是一次几十字节的解码。
	var got []schemeCert
	for _, s := range v23Schemes {
		v, ok := pairs[s.id]
		if !ok {
			continue
		}
		cert, err := firstSignerCert(v)
		if err != nil {
			return nil, fmt.Errorf("%s 签名块：%w", s.name, err)
		}
		if len(cert) > 0 {
			got = append(got, schemeCert{s.name, cert})
		}
	}

	if len(got) == 0 {
		// 没有 v2/v3，这才轮到 v1（规则 4 的放宽版，见包注释的偏差 a）。
		cert, err := v1Cert(r, size)
		if err != nil {
			return nil, err
		}
		if len(cert) > 0 {
			got = append(got, schemeCert{"v1", cert})
		}
	}

	if len(got) == 0 {
		// 空证书与"没有签名"在这里汇成一条：Python 里 `if not cert_encoded` 对 b""
		// 同样为真，所以 fdroidserver 也会把它当成"没取到"。
		return nil, errors.New("没有签名证书 —— 本市场只收上游开发者签名的包")
	}

	// got[0] 是优先级最高的那个（表是有序的），也就是 fdroidserver 会采用的值。
	for _, g := range got[1:] {
		if !bytes.Equal(g.cert, got[0].cert) {
			return nil, fmt.Errorf("%s 与 %s 的签名证书不同 —— 这个 APK 自己就签坏了，"+
				"任何 F-Droid 客户端与 apksigner 都会拒绝它", got[0].scheme, g.scheme)
		}
	}

	sum := sha256.Sum256(got[0].cert)
	return &Signer{SHA256: hex.EncodeToString(sum[:]), Scheme: got[0].scheme}, nil
}

// signingBlockPairs 找出 APK Signing Block，返回它的 ID → 值 映射。没有块时返回 nil。
//
// 块的位置是**从中央目录倒推**出来的（它紧贴中央目录之前），而 `archive/zip` 不暴露
// 中央目录的偏移，所以这里得自己扫 EOCD。整块的结构：
//
//	uint64 块长度（不含这 8 字节）          ← start
//	重复的 pair：uint64 长度 + uint32 ID + 值
//	uint64 块长度（与开头那个**必须相等**）
//	"APK Sig Block 42"（16 字节）          ← 中央目录 - 16
//
// 于是 start = 中央目录 - 8 - 块长度。首尾两个长度字段互为校验：对不上就说明这段
// 字节不是签名块，按"没有块"处理而不是报错 —— 中央目录之前本来就允许有任意数据。
func signingBlockPairs(r io.ReaderAt, size int64) (map[uint32][]byte, error) {
	cdOff, err := centralDirOffset(r, size)
	if err != nil {
		return nil, err
	}

	// 尾部 24 字节 = 块长度(8) + 魔数(16)。中央目录之前放不下它，就是没有块。
	const tailLen = 8 + 16
	if cdOff < tailLen {
		return nil, nil
	}
	var tail [tailLen]byte
	if _, err := r.ReadAt(tail[:], cdOff-tailLen); err != nil {
		return nil, fmt.Errorf("读签名块的尾部：%w", err)
	}
	if string(tail[8:]) != sigBlockMagic {
		return nil, nil
	}
	blockSize := binary.LittleEndian.Uint64(tail[0:8])

	// 块至少要装得下"结尾那个长度字段 + 魔数"，且不能被推成负数。
	if blockSize < tailLen || int64(blockSize)+8 > cdOff {
		return nil, fmt.Errorf("签名块声明的长度 %d 不成立（中央目录在 %d）", blockSize, cdOff)
	}
	start := cdOff - 8 - int64(blockSize)

	var head [8]byte
	if _, err := r.ReadAt(head[:], start); err != nil {
		return nil, fmt.Errorf("读签名块的头部：%w", err)
	}
	if binary.LittleEndian.Uint64(head[:]) != blockSize {
		return nil, nil
	}

	pairs := make([]byte, cdOff-tailLen-start-8)
	if _, err := r.ReadAt(pairs, start+8); err != nil {
		return nil, fmt.Errorf("读签名块：%w", err)
	}
	return parsePairs(pairs)
}

// parsePairs 把块里"长度 + ID + 值"那一串解开。
//
// 长度字段**含**那 4 字节 ID，所以值从第 8 字节起 —— 这是最容易记反的一处。
func parsePairs(b []byte) (map[uint32][]byte, error) {
	out := map[uint32][]byte{}
	for len(b) > 0 {
		if len(b) < 8 {
			return nil, fmt.Errorf("签名块尾部多出 %d 个字节", len(b))
		}
		n := binary.LittleEndian.Uint64(b[0:8])
		if n < 4 || n > uint64(len(b)-8) {
			return nil, fmt.Errorf("签名块里一段的长度 %d 不成立（还剩 %d 字节）", n, len(b)-8)
		}
		v := b[8 : 8+n]
		id := binary.LittleEndian.Uint32(v)
		// 同一个 ID 出现两次不是 AOSP 规范允许的。留**第一份**：fdroidserver 为了
		// 兜住 androguard 的重复插入，专门给 `_v2_blocks` 装了一个不覆写的 dict
		// （common.py:3539-3547），这里从同一个方向兜。
		if _, dup := out[id]; !dup {
			out[id] = v[4:]
		}
		b = b[8+n:]
	}
	return out, nil
}

// centralDirOffset 找出 ZIP 中央目录的起始偏移。
//
// `archive/zip` 不暴露它，而签名块的位置正是由它倒推的，所以只能自己扫：EOCD 记录
// （`PK\x05\x06`）在文件末尾，注释最长 65535 字节，所以它一定落在最后 65557 字节之内。
//
// **必须从后往前**找：ZIP 注释里可以合法地含有 `PK\x05\x06` 这四个字节，那时先命中的
// 是假记录 —— 而假记录的"注释长度"字段对不上它到文件末尾的距离，于是被跳过。
// 真正的 EOCD 是**唯一**满足这个等式的，所以从后往前找到的第一条对得上的就是它。
func centralDirOffset(r io.ReaderAt, size int64) (int64, error) {
	const eocdLen = 22
	const maxComment = 0xFFFF
	if size < eocdLen {
		return 0, fmt.Errorf("文件只有 %d 字节，装不下一个 zip", size)
	}

	n := min(size, int64(eocdLen+maxComment))
	base := size - n
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, base); err != nil {
		return 0, fmt.Errorf("读文件尾部：%w", err)
	}

	for i := len(buf) - eocdLen; i >= 0; i-- {
		if buf[i] != 'P' || buf[i+1] != 'K' || buf[i+2] != 5 || buf[i+3] != 6 {
			continue
		}
		comment := binary.LittleEndian.Uint16(buf[i+20 : i+22])
		if int64(i)+eocdLen+int64(comment) != int64(len(buf)) {
			continue
		}
		off := binary.LittleEndian.Uint32(buf[i+16 : i+20])
		if off == 0xFFFFFFFF {
			// ZIP64：真正的偏移在 ZIP64 EOCD 里，那是另一张记录。走到这里意味着
			// 这个归档超过 4 GiB —— APK 不可能是（4 GiB 的中央目录放不进一个 APK）。
			// 如实报错，而不是猜一个偏移然后拿一段无关的字节当签名块。
			return 0, errors.New("这是一个 ZIP64 归档，不支持")
		}
		return base + int64(off), nil
	}
	return 0, errors.New("找不到 zip 的中央目录（EOCD 记录）")
}

// firstSignerCert 从一个 v2/v3 签名块的值里取出**第一个签名者的第一张证书**。
//
// 布局（AOSP 的 v2 方案文档；androguard 的 `parse_v2_signing_block` 是同一份东西的
// Python 版），所有长度都是 uint32 小端：
//
//	size_sequence                     ← 它 + 4 == 整个值的长度
//	  size_signer
//	    size_signed_data
//	      size_digests + digests
//	      size_certificates
//	        size_cert + cert(DER)       ← 要的就是这一段
//	        …（链上的其余证书，我们不取）
//	      size_attributes + attributes
//	    size_signatures + signatures
//	    size_publickey + publickey
//	  …（可以有多个签名者，我们只要第一个）
//
// 返回 nil 表示"块在、但没有可用的证书"。**不返回错误**：那与 androguard 返回空列表
// 是同一件事，而 fdroidserver 对空列表的处理就是跳过这个方案。
func firstSignerCert(v []byte) ([]byte, error) {
	c := &cursor{b: v}

	n, err := c.u32()
	if err != nil {
		return nil, err
	}
	// androguard 在这里有一条硬校验（"size of sequence and blocksize does not match"）：
	// 头部那个长度必须正好覆盖剩下的全部。抄它 —— 它能挡住"把两个相邻的 pair 看成一个"
	// 这类解析漂移，而漂移的后果是拿一段无关字节去算指纹。
	if int64(n)+4 != int64(len(v)) {
		return nil, fmt.Errorf("长度自相矛盾：头部说 %d 字节，实际有 %d", n, len(v)-4)
	}
	if c.done() {
		return nil, nil // 空序列：一个签名者都没有
	}
	if _, err := c.u32(); err != nil { // size_signer：我们不按它切，只需要跳过
		return nil, err
	}
	signed, err := c.blob()
	if err != nil {
		return nil, err
	}

	d := &cursor{b: signed}
	// digests：不解析。我们不验签，而它唯一的内容就是摘要 —— 留着没有任何用处。
	if err := d.skip(); err != nil {
		return nil, err
	}
	certs, err := d.blob()
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, nil
	}

	// 证书列表是一串"长度 + 证书"。取第一张 —— 这正是 `certs_v2[0]`：
	// fdroidserver 不挑、不验，就是第一个。
	k := &cursor{b: certs}
	return k.blob()
}

// v1Cert 从归档里取出 JAR 签名（v1）的证书。
//
// 判据是 fdroidserver 的那个正则（`common.py` 的 `SIGNATURE_BLOCK_FILE_REGEX`）：
//
//	\AMETA-INF/.*\.(DSA|EC|RSA)\Z
//
// 它锚在两头、且带 `re.DOTALL` —— 所以语义是"整条路径以 `META-INF/` 开头、以这三种
// 后缀之一结尾"，中间是什么都行（**含子目录**）。这里照抄，不收紧。
//
// 多于一个签名块文件时 fdroidserver 直接判错（"Found multiple JAR Signature Block
// Files"），我们也判错：那意味着这个包被签了不止一次，挑哪一个都是猜。
func v1Cert(r io.ReaderAt, size int64) ([]byte, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("读 zip 目录：%w", err)
	}
	var found []*zip.File
	for _, f := range zr.File {
		if isV1BlockName(f.Name) {
			found = append(found, f)
		}
	}
	switch {
	case len(found) == 0:
		return nil, nil
	case len(found) > 1:
		return nil, fmt.Errorf("有 %d 个 JAR 签名块文件（%s、%s…），无法判断签名者是哪一个",
			len(found), found[0].Name, found[1].Name)
	}
	blk, err := readZipEntry(found[0])
	if err != nil {
		return nil, err
	}
	return pkcs7FirstCert(blk)
}

// isV1BlockName 是上面那个正则的等价手写版。
//
// 手写而不用 regexp 有两个理由：一是 Go 的 RE2 里没有 `\Z`（Python 那个"文本绝对结尾"
// 的写法），要等价得写成 `(?s)\AMETA-INF/.*\.(DSA|EC|RSA)\z`，读起来已经不比下面这段
// 清楚；二是这个判断本来只需要**前缀 + 后缀**，不需要真的做一次正则匹配。
//
// 长度上不设任何条件：`META-INF/.RSA` 这种也命中正则（`.*` 可以配空串），所以这里
// 也命中。这种名字不会由任何真实签名器产生，但"与判据一致"比"看着合理"重要。
func isV1BlockName(name string) bool {
	if !strings.HasPrefix(name, "META-INF/") {
		return false
	}
	for _, ext := range []string{".DSA", ".EC", ".RSA"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// readZipEntry 把 zip 里的一个条目整个读出来。
//
// 有上限不是出于安全（那个 zip 就在我们自己的磁盘上），而是为了让失败来得早且明确：
// 一个坏掉的归档可能让 `META-INF/CERT.RSA` 指向几个 GB 的数据，而签名块文件是
// 几 KB 量级的 PKCS#7 —— 撞到上限说明这条路径已经被污染了，该报错而不是继续读。
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("打开 %s：%w", f.Name, err)
	}
	defer rc.Close()

	const maxSignBlock = 1 << 20
	b, err := io.ReadAll(io.LimitReader(rc, maxSignBlock+1))
	if err != nil {
		return nil, fmt.Errorf("读 %s：%w", f.Name, err)
	}
	if len(b) > maxSignBlock {
		return nil, fmt.Errorf("%s 超过 %d 字节，不像是一个 JAR 签名块", f.Name, maxSignBlock)
	}
	return b, nil
}

// cursor 是在一段字节里顺序读 uint32（小端）与"长度前缀的块"的游标。
//
// 反复出现的那个 "读一个长度、再读那么多字节" 在 v2/v3 的布局里出现了七次，
// 单独写成游标是为了让 firstSignerCert 里只剩结构本身，而不是一堆边界判断。
type cursor struct{ b []byte }

func (c *cursor) done() bool { return len(c.b) == 0 }

func (c *cursor) u32() (uint32, error) {
	if len(c.b) < 4 {
		return 0, fmt.Errorf("读到末尾：还要 4 字节的长度，只剩 %d 字节", len(c.b))
	}
	n := binary.LittleEndian.Uint32(c.b)
	c.b = c.b[4:]
	return n, nil
}

// blob 读一个 uint32 长度前缀的块并返回它的内容。返回的切片是底层数组的一段，
// 调用方不该改写它。
func (c *cursor) blob() ([]byte, error) {
	n, err := c.u32()
	if err != nil {
		return nil, err
	}
	if int64(n) > int64(len(c.b)) {
		return nil, fmt.Errorf("块长 %d 超出剩下的 %d 字节", n, len(c.b))
	}
	out := c.b[:n]
	c.b = c.b[n:]
	return out, nil
}

// skip 读一个长度前缀的块并丢掉它，用于"结构上要跨过去、内容不要"的那些段。
func (c *cursor) skip() error {
	_, err := c.blob()
	return err
}
