package elf

type Repo struct {
	*query
}

func New(tableName string, execer Execer) *Repo {
	return &Repo{query: &query{table: tableName, execer: execer}}
}
