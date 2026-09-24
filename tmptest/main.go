package main

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	u := url.QueryEscape("root")
	p := url.QueryEscape("TiDB@2026")
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=true&loc=Local", u, p, "120.92.109.103", 5000, "migration_test", "utf8mb4")
	fmt.Println("dsn:", dsn)
	try(dsn)
	try(fmt.Sprintf("%s:%s@tcp(%s:%d)/", u, p, "120.92.109.103", 5000))
	try(fmt.Sprintf("root:TiDB@2026@tcp(120.92.109.103:5000)/migration_test"))
}

func try(dsn string) {
	db, err := sql.Open("mysql", dsn)
	if err != nil { fmt.Println("open err:", err); return }
	if err := db.Ping(); err != nil { fmt.Println("ping err:", err); return }
	var v int
	db.QueryRow("select 1").Scan(&v)
	fmt.Println("ok:", v)
}
