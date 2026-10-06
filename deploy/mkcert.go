// 一次性自签证书，为把面板绑到局域网/尾网让浏览器不带端口警告。
// SAN 里放**所有**本机可路由 IPv4/IPv6（接口枚举 + 默认路由探测），
// IP 变了重跑一次即可，不用改代码。不走 CA —— 浏览器会警告一次，
// 点"继续"即可（自签的固有代价，正式 CA 只对域名签发，对裸 IP 无解）。
//
// 用法（仓库根目录）：go run deploy/mkcert.go [输出目录，默认 /etc/litepanel/tls]
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// localIPs 枚举全部非回环单播地址，另加回环三件套。
// UDP "connect" 不发包，只是让内核选出默认路由的源地址——覆盖
// "接口枚举漏掉的策略路由"这类边角（Termux/容器里见过）。
func localIPs() []net.IP {
	seen := map[string]bool{}
	var out []net.IP
	add := func(ip net.IP) {
		if ip == nil || seen[ip.String()] {
			return
		}
		seen[ip.String()] = true
		out = append(out, ip)
	}
	if ifs, err := net.Interfaces(); err == nil {
		for _, ifc := range ifs {
			if ifc.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					add(n.IP)
				}
			}
		}
	}
	for _, d := range [][2]string{{"udp", "8.8.8.8:80"}, {"udp6", "[2001:4860:4860::8888]:80"}} {
		c, err := net.Dial(d[0], d[1])
		if err != nil {
			continue
		}
		if ta, ok := c.LocalAddr().(*net.TCPAddr); ok {
			add(ta.IP)
		}
		c.Close()
	}
	// 回环不在接口枚举里保证（lo 地址因环境而异），显式补上。
	// 标准库只给了 IPv6loopback，没给 v4 对应物，直接构造。
	add(net.IPv4(127, 0, 0, 1))
	add(net.IPv6loopback)
	return out
}

func main() {
	dir := "/etc/litepanel/tls"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	ips := localIPs()
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "litepanel"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * 3650 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           ips,
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		panic(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		panic(err)
	}
	fmt.Print("已签发（SAN IP）：")
	for _, ip := range ips {
		fmt.Print(ip.String(), " ")
	}
	fmt.Println("\n证书：", certPath, "\n私钥：", keyPath)
}
