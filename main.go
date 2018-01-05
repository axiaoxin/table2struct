package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	flag "github.com/spf13/pflag"
)

var (
	db                        *sqlx.DB
	useInt64                  bool
	commonInitialisms         = []string{"API", "ASCII", "CPU", "CSS", "DNS", "EOF", "GUID", "HTML", "HTTP", "HTTPS", "ID", "IP", "JSON", "LHS", "QPS", "RAM", "RHS", "RPC", "SLA", "SMTP", "SSH", "TLS", "TTL", "UI", "UID", "UUID", "URI", "URL", "UTF8", "VM", "XML", "XSRF", "XSS"}
	commonInitialismsReplacer *strings.Replacer

	dbHost      string
	dbPort      int
	dbUser      string
	dbPwd       string
	dbName      string
	output      string
	packageName string
	tagGORM     bool
	tagXORM     bool
	tagSQLX     bool
	tagGORMType bool
	tagXORMType bool
	tagJSON     bool
	mapping     []string
	mappingFile string
	//dbMapping 映射关系
	dbMapping map[string]map[string]string
	query     string
)

func init() {
	dbMapping = map[string]map[string]string{
		"global": make(map[string]string),
	}
	var commonInitialismsForReplacer []string
	for _, initialism := range commonInitialisms {
		commonInitialismsForReplacer = append(commonInitialismsForReplacer, strings.ToLower(initialism), initialism)
	}
	commonInitialismsReplacer = strings.NewReplacer(commonInitialismsForReplacer...)

	flag.BoolVar(&useInt64, "int64", false, "是否将tinyint、smallint等类型也转换int64")
	flag.StringVar(&dbHost, "db_host", "127.0.0.1", "数据库ip地址")
	flag.IntVar(&dbPort, "db_port", 3306, "数据库端口")
	flag.StringVar(&dbUser, "db_user", "root", "数据库用户名")
	flag.StringVar(&dbPwd, "db_pwd", "root", "数据库密码")
	flag.StringVar(&dbName, "db_name", "", "数据库名")
	flag.StringVar(&packageName, "package_name", "models", "包名")
	flag.StringVar(&output, "output", "", "输出路径,默认为当前目录")
	flag.BoolVar(&tagGORM, "tag_gorm", false, "是否生成gorm的tag")
	flag.BoolVar(&tagGORMType, "tag_gorm_type", true, "是否将type包含进gorm的tag")
	flag.BoolVar(&tagXORM, "tag_xorm", false, "是否生成xorm的tag")
	flag.BoolVar(&tagXORMType, "tag_xorm_type", true, "是否将type包含进xorm的tag")
	flag.BoolVar(&tagSQLX, "tag_sqlx", false, "是否生成sqlx的tag")
	flag.BoolVar(&tagJSON, "tag_json", true, "是否生成json的tag")
	flag.StringSliceVar(&mapping, "mapping", []string{}, "强制将字段名转换成指定的名称。如--mapping foo:Bar,则表中叫foo的字段在golang中会强制命名为Bar")
	flag.StringVar(&mappingFile, "mapping_file", "", "字段名映射文件")
	flag.StringVar(&query, "query", "", "查询数据库字段名转换后的golang字段名并立即退出")
}

func main() {
	flag.Parse()
	var err error

	//从文件中解析映射规则
	if mappingFile != "" {
		mappingFileContent, err := ioutil.ReadFile(mappingFile)
		if err != nil {
			fmt.Printf("读取映射文件失败:%v\n", err)
			os.Exit(1)
		}
		for _, mappingStr := range strings.Split(string(mappingFileContent), "\n") {
			mappingStr = strings.TrimSpace(mappingStr)
			if mappingStr == "" {
				continue
			}
			if err := addMapping(mappingStr); err != nil {
				fmt.Printf("映射文件格式错误: %v\n", err)
				os.Exit(1)
			}
		}
	}
	//从参数中解析映射规则
	if len(mapping) > 0 {
		for _, mappingStr := range mapping {
			if err := addMapping(mappingStr); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		}
	}

	if query != "" {
		tableName, originName, err := parseQuery(query)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		displayTable := ""
		if tableName != "" {
			displayTable = tableName + "."
		}
		fmt.Println(query, "=>", displayTable+toGoName(originName, tableName))
		return
	}

	if output == "" {
		output, err = filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			fmt.Printf("获取当前路径失败")
			os.Exit(1)
		}
	}

	if _, statErr := os.Stat(output); statErr != nil {
		if os.IsNotExist(statErr) {
			fmt.Printf("错误的输入路径:%v", output)
			os.Exit(1)
		}
	}
	if dbName == "" {
		fmt.Printf("请输入数据库名称")
		os.Exit(1)
	}
	db, err = sqlx.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true", dbUser, dbPwd, dbHost, dbPort, dbName))
	if err != nil {
		fmt.Printf("连接数据库失败:%v", err)
		os.Exit(1)
	}
	defer db.Close()

	var tableNames []string
	if flag.NArg() > 0 {
		tableNames = flag.Args()
	} else {
		if tableNames, err = GetTableNames(); err != nil {
			fmt.Printf("读取表名失败:%v", err)
			os.Exit(1)
		}
	}
	for _, tableName := range tableNames {
		table, err := GetTable(tableName)
		if err != nil {
			fmt.Printf("读取表%v失败:%v\n", tableName, err)
			os.Exit(1)
		}
		tmpFile, err := ioutil.TempFile(os.TempDir(), "table2struct_")
		if err != nil {
			fmt.Println("创建临时文件失败:", err)
			os.Exit(1)
		}
		tmpFile.WriteString(toStruct(table))
		tmpFile.Close()
		defer os.Remove(tmpFile.Name())
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, tmpFile.Name(), nil, parser.ParseComments)
		if err != nil {
			fmt.Println("解析struct失败:", err)
			os.Exit(1)
		}
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, node); err != nil {
			fmt.Printf("格式化%s的代码失败:%v\n", tableName, err)
			os.Exit(1)
		}
		if err = ioutil.WriteFile(filepath.Join(output, tableName+".go"), buf.Bytes(), 0666); err != nil {
			fmt.Printf("保存文件失败:%v\n", err)
			os.Exit(1)
		}
	}

}

//Field 字段
type Field struct {
	Name          string
	Type          string
	OriginType    string
	Length        int
	DecimalDigits int
	IsUnsigned    bool
	EnableNull    bool
	IsPrimaryKey  bool
	Default       sql.NullString
}

//Table 表
type Table struct {
	Name    string
	Fields  []Field
	HasTime bool
}

//TableField 表字段属性
type TableField struct {
	Field      string         `db:"Field"`
	Type       string         `db:"Type"`
	Collation  sql.NullString `db:"Collation"`
	Null       sql.NullString `db:"Null"`
	Key        sql.NullString `db:"Key"`
	Default    sql.NullString `db:"Default"`
	Extra      sql.NullString `db:"Extra"`
	Privileges sql.NullString `db:"Privileges"`
	Comment    sql.NullString `db:"Comment"`
}

//toGoName 参考 github.com/jinzhu/gorm 的 ToDBName
func toGoName(dbName string, tableName string) string {
	if m, ok := dbMapping[tableName]; ok {
		if goName, goNameOK := m[dbName]; goNameOK {
			return goName
		}
	}
	if m, ok := dbMapping["global"]; ok {
		if goName, goNameOK := m[dbName]; goNameOK {
			return goName
		}
	}
	var value string
	for i, v := range dbName {
		if (v >= 'A' && v <= 'Z') || (v >= 'a' && v <= 'z') {
			value = dbName[i:]
			break
		}
	}
	value = commonInitialismsReplacer.Replace(value)
	buf := bytes.NewBufferString("")
	for i, v := range value[:len(value)-1] {
		if i > 0 {
			if v == '_' || v == '-' {
				continue
			}
			if value[i-1] == '_' {
				buf.WriteRune(unicode.ToUpper(v))
			} else {
				buf.WriteRune(v)
			}
		} else {
			buf.WriteRune(unicode.ToUpper(v))
		}
	}
	buf.WriteByte(value[len(value)-1])
	return buf.String()
}

//GetTableNames 获取所有表名
func GetTableNames() ([]string, error) {
	tables := make([]string, 0, 32)
	rows, err := db.Query("SHOW TABLES")
	if err != nil {
		return tables, err
	}
	var tableName string
	for rows.Next() {
		if err = rows.Scan(&tableName); err != nil {
			return tables, err
		}
		tables = append(tables, tableName)
	}
	return tables, nil
}

//GetTable 获取表
func GetTable(tableName string) (Table, error) {
	table := Table{
		Fields: make([]Field, 0, 16),
	}
	table.Name = tableName
	rows, err := db.Queryx("DESC `" + tableName + "`")
	if err != nil {
		return table, err
	}
	var tableField TableField
	for rows.Next() {
		if err = rows.StructScan(&tableField); err != nil {
			return table, err
		}
		field := ParseField(tableField)
		if field.Type == "time.Time" {
			table.HasTime = true
		}
		table.Fields = append(table.Fields, field)
	}
	return table, nil
}

const (
	tableTpl = `
package %s

%s

//%s %s
type %s struct {
%s
}

//TableName %s
func (t *%s) TableName() string {
	return "%s"
}`
)

//toStruct 将表转换为struct字符串
func toStruct(table Table) string {
	buf := bytes.NewBufferString("")
	for _, field := range table.Fields {
		buf.WriteString("\t" + toGoName(field.Name, table.Name) + "\t" + field.Type)
		tags := make([]string, 0)
		if tagJSON {
			tags = append(tags, `json:"`+field.Name+`"`)
		}
		if tagSQLX {
			tags = append(tags, `db:"`+field.Name+`"`)
		}
		if tagGORM {
			gormTags := []string{"column:" + field.Name}
			if tagGORMType {
				if strings.Contains(field.OriginType, ")") {
					gormTags = append(gormTags, "type:"+field.OriginType[:strings.Index(field.OriginType, ")")+1])
				} else {
					gormTags = append(gormTags, "type:"+field.OriginType)
				}
			}
			tags = append(tags, fmt.Sprintf(`gorm:"%s"`, strings.Join(gormTags, ";")))
		}
		if tagXORM {
			xormTags := []string{"'" + field.Name + "'"}
			if tagXORMType {
				if strings.Contains(field.OriginType, ")") {
					xormTags = append(xormTags, field.OriginType[:strings.Index(field.OriginType, ")")+1])
				} else {
					xormTags = append(xormTags, field.OriginType)
				}
			}
			tags = append(tags, fmt.Sprintf(`xorm:"%s"`, strings.Join(xormTags, " ")))
		}
		if len(tags) > 0 {
			tag := strings.Join(tags, " ")
			buf.WriteString(" `" + tag + "`")
		}
		buf.WriteRune('\n')
	}
	tableGoName := toGoName(table.Name, table.Name)
	importString := "\n"
	if table.HasTime {
		importString = `
import (
	"time"
)`
	}
	return fmt.Sprintf(tableTpl, packageName, importString, tableGoName, table.Name, tableGoName, buf.String(), table.Name, tableGoName, table.Name)
}

//ParseField 解析字段
func ParseField(tField TableField) Field {
	var field Field
	attrs := strings.Split(tField.Type, " ")
	var t string
	for _, attr := range attrs {
		attr = strings.ToLower(attr)
		if attr == "unsigned" {
			field.IsUnsigned = true
		} else if strings.Contains(attr, "(") && strings.Contains(attr, ")") {
			l := strings.Index(attr, "(")
			if l > 0 {
				t = attr[0:l]
			}
		} else {
			t = attr
		}
	}
	if tField.Null.String == "NULL" {
		field.EnableNull = true
	}
	if tField.Key.String == "PRI" {
		field.IsPrimaryKey = true
	}
	field.Name = tField.Field
	field.Type = goType(t)
	field.Default = tField.Default
	if field.IsUnsigned && !useInt64 {
		field.Type = "u" + field.Type
	}
	field.OriginType = tField.Type
	return field
}

func goType(dbType string) string {
	switch dbType {
	case "tinyint":
		if useInt64 {
			return "int64"
		}
		return "int8"
	case "smallint":
		fallthrough
	case "mediumint":
		fallthrough
	case "integer":
		fallthrough
	case "int":
		if useInt64 {
			return "int64"
		}
		return "int"
	case "bigint":
		return "int64"
	case "float":
		fallthrough
	case "double":
		fallthrough
	case "decimal":
		fallthrough
	case "numeric":
		return "float64"
	case "bool":
		return "bool"
	case "char":
		fallthrough
	case "varchar":
		fallthrough
	case "tinytext":
		fallthrough
	case "text":
		fallthrough
	case "mediumtext":
		fallthrough
	case "longtext":
		return "string"
	case "date":
		fallthrough
	case "datetime":
		fallthrough
	case "time":
		fallthrough
	case "timestamp":
		return "time.Time"
	default:
		panic("未知类型:" + dbType)
		//return ""
	}
}

//addMapping 增加映射
func addMapping(m string) error {
	if strings.Count(m, ":") != 1 {
		return fmt.Errorf("映射格式错误: [%s]", m)
	}
	m1 := strings.Split(m, ":")
	if len(m1) != 2 {
		return fmt.Errorf("映射格式错误: [%s]", m)
	}
	destName := m1[1]
	var originName string
	tableName := "global"
	if strings.Contains(m1[0], ".") {
		m2 := strings.Split(m1[0], ".")
		if len(m2) != 2 {
			return fmt.Errorf("映射格式错误: [%s]", m)
		}
		tableName, originName = m2[0], m2[1]
	} else {
		originName = m1[0]
	}
	if _, ok := dbMapping[tableName]; !ok {
		dbMapping[tableName] = make(map[string]string)
	}
	dbMapping[tableName][originName] = destName
	return nil
}

func parseQuery(query string) (tableName, fieldName string, err error) {
	if strings.Contains(query, ".") {
		q := strings.Split(query, ".")
		if len(q) != 2 {
			err = fmt.Errorf("格式错误")
		}
		tableName = q[0]
		fieldName = q[1]
	} else {
		fieldName = query
	}
	return
}
