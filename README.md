
---

#  Kami Tunnel TCP/udp NO LIMIT TRAFFIC 
## nâng cấp zero copy & kcpu udp tunnel cần src hãy mở issues

#  chú ý : vui lòng tắt rate limit in 127.0.0.1 nhé 

Kami Tunnel là công cụ tạo **đường hầm (tunnel)** giúp bạn **public port nội bộ ra Internet** nhanh chóng, an toàn và đơn giản — tương tự ngrok nhưng gọn nhẹ hơn.

---

## 📦 Bản phát hành mới nhất

Tải bản tương ứng với hệ điều hành của bạn:

| Hệ điều hành              | Kiến trúc | Tải xuống                                                                                                                       | SHA-256           |
| ------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------- | ----------------- |
| **Linux**                 | amd64     | [kami-tunnel-linux-amd64.tar.gz](https://github.com/kami2k1/tunnel/releases/latest/download/kami-tunnel-linux-amd64.tar.gz)     | *(đang cập nhật)* |
| **Linux**                 | arm64     | [kami-tunnel-linux-arm64.tar.gz](https://github.com/kami2k1/tunnel/releases/latest/download/kami-tunnel-linux-arm64.tar.gz)     | *(đang cập nhật)* |
| **Windows**               | amd64     | [kami-tunnel-windows-amd64.tar.gz](https://github.com/kami2k1/tunnel/releases/latest/download/kami-tunnel-windows-amd64.tar.gz) | *(đang cập nhật)* |
| **Windows**               | arm64     | [kami-tunnel-windows-arm64.tar.gz](https://github.com/kami2k1/tunnel/releases/latest/download/kami-tunnel-windows-arm64.tar.gz) | *(đang cập nhật)* |
| **macOS (Intel)**         | amd64     | [kami-tunnel-darwin-amd64.tar.gz](https://github.com/kami2k1/tunnel/releases/latest/download/kami-tunnel-darwin-amd64.tar.gz)   | *(đang cập nhật)* |
| **macOS (Apple Silicon)** | arm64     | [kami-tunnel-darwin-arm64.tar.gz](https://github.com/kami2k1/tunnel/releases/latest/download/kami-tunnel-darwin-arm64.tar.gz)   | *(đang cập nhật)* |
| **Android (Termux)**      | arm64     | [kami-tunnel-android-arm64.tar.gz](https://github.com/kami2k1/tunnel/releases/latest/download/kami-tunnel-android-arm64.tar.gz) | *(đang cập nhật)* |

---

## ⚙️ Cách sử dụng
###  Niếu port là udp ví dụ như game minecraft PE  thì thêm --proto udp nhé 
# bắt buộc thêm flag --host 127.0.0.1 đối với minecraft nhé không là ko jon được mình đã tets 


ví dụ:

```
./kami-tunnel --proto udp --host 127.0.0.1 19132 
trong đó 19132 là port ngốc udp là protoco 

```

---
### 🐧 Linux / macOS

Giải nén và chạy:

```
tar -xzf kami-tunnel-linux-amd64.tar.gz
chmod +x kami-tunnel
./kami-tunnel 80
```

---

### 🪟 Windows

Giải nén và chạy:

```
tar -xzf kami-tunnel-windows-amd64.tar.gz
kami-tunnel.exe 80
```

---

### 📱 Android (Termux)

Giải nén và chạy:

```
tar -xzf kami-tunnel-android-arm64.tar.gz
chmod +x kami-tunnel
./kami-tunnel 80
```

---

## 🔐 Tùy chọn nâng cao

* ` <PORT>`: chỉ định cổng nội bộ (nếu không truyền mặc định là 80).
* ` --host` : mặc định  localhost ko biết đừng đổi 


Ví dụ:

```
./kami-tunnel  80 
trong đó 80 là port được nat ra ngoài 
```

---

## 📜 Giấy phép

Phát hành theo giấy phép **MIT License**
© 2025 – Kami2k1 (Quang Dev)

---