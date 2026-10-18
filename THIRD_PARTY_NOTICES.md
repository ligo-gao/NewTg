# Third-Party Notices

NEW TG 的源代码为自研代码。本文件收录随源码/二进制分发的第三方**数据**及其
许可文本，以满足许可证的署名与许可文本保留要求。

## SecLists — `Passwords/Common-Credentials/10k-most-common.txt`

`backend/internal/auth/password.go` 内嵌了 SecLists 仓库中
`Passwords/Common-Credentials/10k-most-common.txt` 按发布顺序的前 3000 行，
用作离线高频口令表（ASVS 6.2.4 / security-01 §2.3）。该数据仅随源码发布、
仅用于口令强度筛查，不构成对 NEW TG 源代码的许可约束。

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
