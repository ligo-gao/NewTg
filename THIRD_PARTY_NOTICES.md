# Third-Party Notices

NEW TG 的源代码为自研代码。本文件收录随源码/二进制分发的第三方**数据**及其
许可文本，以满足许可证的署名与许可文本保留要求。运行镜像内同一文件位于
`/usr/share/licenses/newtg/THIRD_PARTY_NOTICES.md`（`backend/Dockerfile` 的
`COPY` 步骤），release 页面另附一份可独立下载的副本。

## SecLists — `Passwords/Common-Credentials/10k-most-common.txt`

`backend/internal/auth/password.go` 内嵌了 SecLists 仓库中
`Passwords/Common-Credentials/10k-most-common.txt` 按发布顺序的前 3000 行，
用作离线高频口令表（ASVS 6.2.4 / security-01 §2.3）。该数据随源码与二进制/镜像
分发（这 3000 行已编译进 `/newtg`），仅用于口令强度筛查，不构成对 NEW TG 源代码
的许可约束。

来源仓库：<https://github.com/danielmiessler/SecLists>

```
MIT License

Copyright (c) 2018 Daniel Miessler

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## qrcode.react — `frontend/package.json` (`^4`, 4.2.0)

M1 的 2FA 设置弹窗用 `qrcode.react` 把 `otpauth://` URI 渲染为纯 SVG 二维码。
该包没有 npm 运行时依赖（仅 peer `react`），并内嵌 QR Code Generator
（Project Nayuki，MIT）。

来源仓库：<https://github.com/zpao/qrcode.react>

```
ISC License

Copyright (c) 2015, Paul O'Shannessy

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY AND
FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR
OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
PERFORMANCE OF THIS SOFTWARE.
```

内嵌 QR Code Generator（`src/third-party/qrcodegen`）：

```
MIT License

Copyright (c) Project Nayuki

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
