package tools

import "encoding/json"

type projectNoteUpdateArgs struct {
	Note string `json:"note"`
}

func (r *Registry) projectNoteUpdate(raw json.RawMessage) Result {
	args, err := decodeArgs[projectNoteUpdateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Note == "" {
		return Result{OK: false, Error: "note is required"}
	}
	r.projectNote.Note = args.Note
	return Result{OK: true, Data: r.projectNote, Message: "project note updated"}
}

func formatProjectNote(note ProjectNote) string {
	if note.Note == "" {
		return "暂无项目笔记。规划阶段必须调用 project_note_update 维护详细自由文本笔记，像人工审计员一样记录项目架构、行为、登录认证、鉴权、攻击面、数据/状态流、关键文件角色、已知结论和待确认问题。\n"
	}
	return note.Note + "\n"
}
