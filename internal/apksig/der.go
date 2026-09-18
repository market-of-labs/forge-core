package apksig

import (
	"errors"
	"fmt"
)

// 本文件是那个手搓 DER 遍历器。它存在的理由与"不用第三方库"的一贯理由相同，
// 但在这里格外站得住：**要取的那一段本来就是 DER**。
//
// fdroidserver 走的是 asn1crypto（`cms.ContentInfo.load(...)` 然后
// `['content']['certificates'][0].chosen.dump()`），那是一个完整的 ASN.1 库 —— 它建了一棵
// 带类型的对象树，再 `dump()` 回去。而对我们的需求来说，"dump 回去"恰恰是**多余的一步**：
// SignedData 里躺着的就是原样的证书字节，我们要的只是"顺着标签走到那一层、把第一段
// TLV 整段拿出来"。
//
// 少掉的那一步还有一个实际好处：asn1crypto 的 `dump()` 是**重新编码**，理论上可能与
// 原字节不同（非规范 DER 的输入）。而 sha256 恰恰是对字节算的 —— 直接切原字节，
// 等于把这个风险从"理论上存在"变成"不存在"。
//
// 代价是这里没有类型系统：`readTLV` 只认单字节标签、只认定长编码，别的都当解析失败。
// 而这正是我们要的严格度 —— RFC 5652 里的证书、SEQUENCE、SET、OID、[0] 全是单字节标签。

// DER 标签（只列本文件用到的）。
const (
	tagOID      = 0x06 // OBJECT IDENTIFIER
	tagSequence = 0x30 // SEQUENCE
	// tagContext0 是 [0] 这个上下文标签号。本文件里它在**两处**出现，含义不同：
	//
	//	ContentInfo.content  [0] EXPLICIT Any        —— 内容是 SignedData 那一整段
	//	SignedData.certificates [0] IMPLICIT CertificateSet —— 内容直接是一张张证书
	//
	// 两处都只用同一个标签号去认，但**取内容的方式不同**（前者还要再读一层 TLV，
	// 后者的内容就是证书本身）。这个差别是 EXPLICIT 与 IMPLICIT 的差别，也是最容易
	// 写错的一处：把 IMPLICIT 当成 EXPLICIT，解析会多走一层 —— 而多走的那一层恰好
	// 也是一段合法 TLV 的开头，于是取到的是一段垃圾，还不报错。
	tagContext0 = 0xA0
)

// tlv 是一段 DER 的 TLV。
type tlv struct {
	tag byte
	// full 是**含头部**的完整字节。证书 DER 取的就是它 —— 证书的 DER 编码本来就
	// 包含它自己的 SEQUENCE 头部，而不是只有内容。
	full []byte
	// content 是不含头部的内容。
	content []byte
}

// readTLV 读一段 DER TLV，返回它以及它占了几个字节。
//
// 不支持高位标签号（首字节低 5 位全 1）与不定长编码 —— DER 里两者都不该出现，
// 遇到就当解析失败。也不做任何语义解释：这个函数不知道也不关心读的是不是证书。
func readTLV(b []byte) (t tlv, consumed int, err error) {
	if len(b) < 2 {
		return tlv{}, 0, errors.New("不足 2 字节，构不成一个 TLV")
	}
	tag := b[0]
	if tag&0x1F == 0x1F {
		return tlv{}, 0, fmt.Errorf("不支持高位标签号（首字节 0x%02X）", tag)
	}
	n, hdr, err := readDERLen(b[1:])
	if err != nil {
		return tlv{}, 0, err
	}
	consumed = 1 + hdr + n
	if consumed > len(b) {
		return tlv{}, 0, fmt.Errorf("TLV 声明有 %d 字节内容，但只剩 %d 字节", n, len(b)-1-hdr)
	}
	return tlv{tag: tag, full: b[:consumed], content: b[1+hdr : consumed]}, consumed, nil
}

// readDERLen 读 DER 的长度字段，返回长度与**长度字段本身**占的字节数。
//
// 短形式（首字节 < 0x80）里长度就写在那一字节里；长形式（首字节高位置 1）后面跟着
// (首字节 & 0x7F) 个字节的大端长度。0x80（不定长）在 DER 里被禁止，这里也拒掉。
func readDERLen(b []byte) (length, n int, err error) {
	if len(b) == 0 {
		return 0, 0, errors.New("TLV 没有长度字段")
	}
	if b[0] < 0x80 {
		return int(b[0]), 1, nil
	}
	k := int(b[0] & 0x7F)
	if k == 0 {
		return 0, 0, errors.New("DER 里不该出现不定长编码（0x80）")
	}
	if k > 4 {
		// 4 字节够到 4 GiB，远超一个 APK 里的任何一段。更大的长度只可能来自
		// "把一段无关数据当成了 TLV" —— 那时报错比继续算下去有用。
		return 0, 0, fmt.Errorf("长度字段占 %d 字节，超出本函数的支持范围", k)
	}
	if len(b) < 1+k {
		return 0, 0, errors.New("TLV 的长度字段被截断")
	}
	v := 0
	for _, c := range b[1 : 1+k] {
		v = v<<8 | int(c)
	}
	return v, 1 + k, nil
}

// pkcs7FirstCert 从 PKCS#7 的 SignedData 里取出第一张证书的 DER。
//
// 走法（RFC 5652）：
//
//	ContentInfo ::= SEQUENCE {
//	    contentType  OBJECT IDENTIFIER,          -- 1.2.840.113549.1.7.2 = signedData
//	    content  [0] EXPLICIT ANY }
//	SignedData ::= SEQUENCE {
//	    version          CMSVersion,
//	    digestAlgorithms SET OF DigestAlgorithmIdentifier,
//	    encapContentInfo SEQUENCE { … },
//	    certificates [0] IMPLICIT CertificateSet OPTIONAL,   ← 要的就是这一层
//	    crls         [1] IMPLICIT RevocationInfoChoices OPTIONAL,
//	    signerInfos  SET OF SignerInfo }
//	CertificateSet ::= SET OF CertificateChoices
//
// 找 `certificates` 用的是"在 SignedData 的**直接子元素**里找第一个 0xA0"，而不是
// "跳过前三个、取第四个"。理由是 SignedData 里唯一可能带 [0] 标签的只有它
// （crls 是 [1]，即 0xA1），而"前三个"这个数法依赖上面那张表被一个字段不漏地抄对 ——
// 抄错（比如漏掉 digestAlgorithms）的症状是解析安稳地停在另一个字段上、然后拿一段
// 看起来完全正常的字节当证书。按标签找没有这个失败模式。
//
// 只取第一张：这正是 fdroidserver 的 `certificates[0]`。但**多于一张时报错**，
// 不静默取第一张 —— 见包注释的偏差 b：那是证书链，真正签名的是哪一张需要验证签名
// 才知道，fdroidserver 为此专门写了一个 `get_jar_signer_certificate`。
func pkcs7FirstCert(der []byte) ([]byte, error) {
	ci, _, err := readTLV(der)
	if err != nil {
		return nil, fmt.Errorf("解析 PKCS#7：%w", err)
	}
	if ci.tag != tagSequence {
		return nil, fmt.Errorf("PKCS#7 的开头不是 SEQUENCE（标签 0x%02X）", ci.tag)
	}

	// ContentInfo 的两个子元素：先是 contentType 那个 OID，再是 content。
	oid, n, err := readTLV(ci.content)
	if err != nil {
		return nil, fmt.Errorf("解析 PKCS#7 的 ContentInfo：%w", err)
	}
	if oid.tag != tagOID {
		return nil, fmt.Errorf("PKCS#7 的 ContentInfo 第一个子元素不是 OID（标签 0x%02X）", oid.tag)
	}
	// OID 的值不校验：走到这里已经说明这个文件被当成 JAR 签名块了，而"它到底是不是
	// signedData"要连 OID 的字节一起比才说得准 —— 比错了只会把能读的包判死。
	// 真正需要挡住的是"结构读不懂"，那由下面几层标签与长度校验来挡。
	explicit, _, err := readTLV(ci.content[n:])
	if err != nil {
		return nil, fmt.Errorf("解析 PKCS#7 的 content：%w", err)
	}
	if explicit.tag != tagContext0 {
		// 这里复用 tagContext0 只是因为 [0] 这个标签号相同，含义完全不同：这一层是
		// ContentInfo 的 `content [0] EXPLICIT`，里面是一个 SignedData。
		return nil, fmt.Errorf("PKCS#7 里没有 content（标签 0x%02X）", explicit.tag)
	}

	sd, _, err := readTLV(explicit.content)
	if err != nil {
		return nil, fmt.Errorf("解析 PKCS#7 的 SignedData：%w", err)
	}
	if sd.tag != tagSequence {
		return nil, fmt.Errorf("PKCS#7 的 content 不是 SEQUENCE（标签 0x%02X）", sd.tag)
	}

	for b := sd.content; len(b) > 0; {
		el, used, err := readTLV(b)
		if err != nil {
			return nil, fmt.Errorf("解析 SignedData 的子元素：%w", err)
		}
		if el.tag != tagContext0 {
			b = b[used:]
			continue
		}
		cert, used, err := readTLV(el.content)
		if err != nil {
			return nil, fmt.Errorf("解析 SignedData 的证书：%w", err)
		}
		if used != len(el.content) {
			return nil, errors.New("这个 JAR 签名里带的是证书链（多张证书）。" +
				"fdroidserver 会验证签名挑出真正签名的那一张，我们不做这件事 —— " +
				"随便取一张的后果是把一张中间证书的指纹当成签名者记下来，" +
				"而那个错值看起来完全正常。需要它的话请人工确认")
		}
		return cert.full, nil
	}
	return nil, errors.New("PKCS#7 的 SignedData 里没有证书")
}
