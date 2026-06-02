你调用了 end_audit，但仍有 unseen/reviewing 文件。请先确认这些文件是否确实无需继续审计。

!{files}

如果这些文件已经确认不需要继续审计，请先简短说明理由，然后再次调用 end_audit；第二次调用将允许结束。如果还需要审计，请继续调用 read_file/search_content/file_review_update。
