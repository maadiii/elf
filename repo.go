package elf

type Repo struct {
	*query
}

func New(tableName string) *Repo {
	return &Repo{query: &query{table: tableName}}
}
