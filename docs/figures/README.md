# docs/figures — chuẩn diagram của repo này

Sinh bằng skill `diagram-design` (44 type · HTML+SVG tự chứa · editorial). Chuẩn áp dụng cho **mọi project**.
Bản đầy đủ: `_rules/diagram-standard.md` (ai-memory) · nghiên cứu nền: `~/.pi/agent/docs/diagram-standard-faang-20261009.md`.

## Lưu ở đâu
| Thứ | File | Khi nào |
|---|---|---|
| Source (bắt buộc) | `<topic>.html` | luôn — tự chứa, commit vào git, là source of truth |
| Bản gửi | `<topic>.png` | khi gửi chat/Telegram/docs (PNG @2) |
| Vector | `<topic>.svg` | chỉ khi cần Figma/Illustrator |
| Gallery | `index.html` | khi >=3 hình (`diagram-scaffold.sh --yes --index`) |

Tên `<topic>`: kebab-case, nói chủ thể (`player-join-flow`, `nakama-module-map`). **Không** ngày/version — provenance là `git log`.
Brand riêng của repo: `.diagram-design` ở repo root chứa `profile: <slug>`; brand ở `~/.diagram-design/profiles/<slug>.md`.

## Chọn tầng trước, type sau (C4)
Context (hệ thống nằm ở đâu) → Container (mấy đơn vị deploy được, giao thức gì) → Component (trong 1 service có gì) → Code (chỉ khi thuật toán lạ).
Runtime (sequence/state machine) và Change (architecture-delta) là trục riêng. **Context + container là đủ cho hầu hết team.** 1 hình = 1 câu hỏi.

## 8 luật khi hình vào docs (theo Google developer docs style guide)
1. **Câu dẫn đầy đủ trước hình** (`:` nếu hình ngay sau, `.` nếu có đoạn đệm).
2. **Caption đánh số**: `**Hình N**. Mô tả ngắn.` — đánh số theo *tài liệu*, không theo file.
3. **Alt text cho MỌI hình**; hình phức tạp → thêm 1 câu mô tả dài trong prose; hình trang trí → `alt=""`.
4. **Nền `paper`, KHÔNG nền trong suốt** cho hình vào docs (chỉ slide màu mới dùng nền trong suốt).
5. Prefer **SVG**; PNG cho bản gửi; cặp 1x/2x cho web; không GIF động.
6. **Cấm ảnh của chữ/code/terminal** — dùng chữ thật (SVG ở đây chứa text thật).
7. Không căn giữa hình · không nhét `<img>` trong `<p>` · nhất quán trong cả doc set.
8. **Cấm secret/PII trong hình.** Buộc che → phủ khối đặc 100% opacity; **cấm blur/mosaic** (khôi phục được).

## Sống với code
Hình để **cạnh doc mà nó phục vụ**, commit cùng PR, **giữ file `.html` nguồn** để regenerate. Không vẽ lại đầy đủ schema/API — chỉ phần liên quan tới quyết định. Thiết kế đổi → `architecture-delta` thay vì vẽ lại.

## Meta bắt buộc + gate freshness
Mỗi hình khai báo nó document cái gì, ngay trong file (vô hình khi render):
```html
<!-- diagram-meta {"topic":"player-join-flow","level":"runtime","type":"sequence","question":"Điều gì xảy ra khi client join?","sources":["src/net/*.go","proto/*.proto"],"updated":"YYYY-MM-DD"} -->
```
```bash
bash ~/.pi/agent/scripts/diagram-freshness.sh [--repo .] [--json]   # exit 1 = còn hình phải sync
```
Trạng thái: `FRESH` (nguồn không đổi sau hình) · `STALE` (nguồn commit sau hình) · `TOUCHED` (nguồn đang dirty) · `NO-META` (hình cũ thiếu meta) · `SOURCES-MISSING` (glob không match).
**Sửa logic/thiết kế ⇒ sync hình trong cùng PR.** `sources` phải là glob của code thật sự quyết định nội dung hình — không phải cả repo.

## Lệnh
```bash
SKILL_DIR=$(find ~/.pi/agent -maxdepth 7 -type d -path "*diagram-design/skills/diagram-design" | head -1)
python3 "$SKILL_DIR/scripts/self_check.py" <topic>.html          # phải in OK trước khi commit
/export-diagram <topic>.html                                      # xuất PNG/SVG (thủ công, không tự chạy)
bash ~/.pi/agent/scripts/diagram-scaffold.sh --yes [--index] [--profile <slug>]
```

