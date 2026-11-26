package cli

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

var dbeaverLocalKey = []byte{
	0xba, 0xbb, 0x4a, 0x9f, 0x77, 0x4a, 0xb8, 0x53,
	0xc9, 0x6c, 0x2d, 0x65, 0x3d, 0xfe, 0x54, 0x4a,
}

var nonFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeFilename(name string) string {
	cleaned := nonFilenameChars.ReplaceAllString(name, "-")
	cleaned = strings.TrimLeft(cleaned, ".")
	if cleaned == "" {
		return "branch"
	}
	return cleaned
}

func deterministicUUID(seed string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

func marshalXML(header string, v any) ([]byte, error) {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(header)+len(body)+1)
	out = append(out, header...)
	out = append(out, body...)
	out = append(out, '\n')
	return out, nil
}

type dbeaverMetaArchive struct {
	XMLName   xml.Name             `xml:"archive"`
	Version   string               `xml:"version,attr"`
	Source    dbeaverMetaSource    `xml:"source"`
	Libraries struct{}             `xml:"libraries"`
	Projects  []dbeaverMetaProject `xml:"projects>project"`
}

type dbeaverMetaSource struct {
	Time    string `xml:"time,attr"`
	Address string `xml:"address,attr"`
	Host    string `xml:"host,attr"`
}

type dbeaverMetaProject struct {
	Name        string                `xml:"name,attr"`
	Description string                `xml:"description,attr"`
	Resources   []dbeaverMetaResource `xml:"resource"`
}

type dbeaverMetaResource struct {
	Name      string                `xml:"name,attr"`
	Resources []dbeaverMetaResource `xml:"resource,omitempty"`
}

type dbeaverProjectDesc struct {
	XMLName   xml.Name `xml:"projectDescription"`
	Name      string   `xml:"name"`
	Comment   string   `xml:"comment"`
	Projects  struct{} `xml:"projects"`
	BuildSpec struct{} `xml:"buildSpec"`
	Natures   []string `xml:"natures>nature"`
}

type dataGripProject struct {
	XMLName   xml.Name          `xml:"project"`
	Version   string            `xml:"version,attr"`
	Component dataGripComponent `xml:"component"`
}

type dataGripComponent struct {
	Name           string             `xml:"name,attr"`
	Format         string             `xml:"format,attr"`
	MultifileModel string             `xml:"multifile-model,attr"`
	DataSource     dataGripDataSource `xml:"data-source"`
}

type dataGripDataSource struct {
	Source      string `xml:"source,attr"`
	Name        string `xml:"name,attr"`
	UUID        string `xml:"uuid,attr"`
	DriverRef   string `xml:"driver-ref"`
	Synchronize bool   `xml:"synchronize"`
	JDBCDriver  string `xml:"jdbc-driver"`
	JDBCURL     string `xml:"jdbc-url"`
	UserName    string `xml:"user-name"`
	WorkingDir  string `xml:"working-dir"`
}

type navicatConnections struct {
	XMLName    xml.Name          `xml:"Connections"`
	Ver        string            `xml:"Ver,attr"`
	Connection navicatConnection `xml:"Connection"`
}

type navicatConnection struct {
	ConnectionName string `xml:"ConnectionName,attr"`
	ConnType       string `xml:"ConnType,attr"`
	Host           string `xml:"Host,attr"`
	Port           int    `xml:"Port,attr"`
	Database       string `xml:"Database,attr"`
	UserName       string `xml:"UserName,attr"`
	SSH            string `xml:"SSH,attr"`
	SavePassword   string `xml:"SavePassword,attr"`
}

type workbenchData struct {
	XMLName   xml.Name      `xml:"data"`
	GRTFormat string        `xml:"grt_format,attr"`
	List      workbenchList `xml:"value"`
}

type workbenchList struct {
	Type              string           `xml:"type,attr"`
	ContentType       string           `xml:"content-type,attr"`
	ContentStructName string           `xml:"content-struct-name,attr"`
	Connection        workbenchConnObj `xml:"value"`
}

type workbenchConnObj struct {
	Type           string `xml:"type,attr"`
	StructName     string `xml:"struct-name,attr"`
	ID             string `xml:"id,attr"`
	StructChecksum string `xml:"struct-checksum,attr"`
	Items          []any
}

type workbenchLink struct {
	XMLName    xml.Name `xml:"link"`
	Type       string   `xml:"type,attr"`
	StructName string   `xml:"struct-name,attr"`
	Key        string   `xml:"key,attr"`
	Text       string   `xml:",chardata"`
}

type workbenchValue struct {
	XMLName  xml.Name         `xml:"value"`
	Type     string           `xml:"type,attr"`
	Key      string           `xml:"key,attr"`
	Text     string           `xml:",chardata"`
	Children []workbenchValue `xml:"value,omitempty"`
}

func fileStem(conn branchConnection) string {
	stem := sanitizeFilename(conn.Branch)
	if conn.ViaProxy {
		return stem + "-proxysql"
	}
	return stem
}

func connDisplayName(conn branchConnection) string {
	if conn.ViaProxy {
		return conn.Branch + " (ProxySQL)"
	}
	return conn.Branch
}

func encodeURIComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func jdbcBaseURL(conn branchConnection) string {
	scheme := conn.Engine
	if scheme == "postgres" {
		scheme = "postgresql"
	}
	dbPart := ""
	if conn.Database != "" {
		dbPart = encodeURIComponent(conn.Database)
	}
	return fmt.Sprintf("jdbc:%s://%s/%s", scheme, formatHostPort(conn.Host, conn.Port), dbPart)
}

func buildJDBCURL(conn branchConnection, includePassword bool) string {
	params := []string{"user=" + encodeURIComponent(conn.User)}
	if includePassword && conn.Password != "" {
		params = append(params, "password="+encodeURIComponent(conn.Password))
	}
	return jdbcBaseURL(conn) + "?" + strings.Join(params, "&")
}

func buildTablePlusURL(conn branchConnection, includePassword bool) string {
	pwPart := ""
	if includePassword && conn.Password != "" {
		pwPart = ":" + encodeURIComponent(conn.Password)
	}
	dbPart := ""
	if conn.Database != "" {
		dbPart = encodeURIComponent(conn.Database)
	}
	return fmt.Sprintf("%s://%s%s@%s/%s",
		conn.Engine, encodeURIComponent(conn.User), pwPart, formatHostPort(conn.Host, conn.Port), dbPart)
}

func encryptDBeaverCredentials(plaintextJSON string) ([]byte, error) {
	raw := []byte(plaintextJSON)
	pad := aes.BlockSize - (len(raw) % aes.BlockSize)
	padded := make([]byte, len(raw)+pad)
	copy(padded, raw)
	for i := len(raw); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(dbeaverLocalKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, aes.BlockSize+len(padded))
	copy(out[:aes.BlockSize], iv)
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(out[aes.BlockSize:], padded)
	return out, nil
}

func buildDBeaverDBP(conn branchConnection, includePassword bool) ([]byte, error) {
	var provider, driver string
	switch conn.Engine {
	case "mysql":
		provider, driver = "mysql", "mysql8"
	case "mariadb":
		provider, driver = "mysql", "mariaDB"
	case "postgres":
		provider, driver = "postgresql", "postgres-jdbc"
	default:
		return nil, fmt.Errorf("dbeaver format does not support engine %q", conn.Engine)
	}
	stem := fileStem(conn)
	proj := "db-branch-" + stem
	connID := proj

	dsJSON, _ := json.MarshalIndent(map[string]any{
		"folders": map[string]any{},
		"connections": map[string]any{
			connID: map[string]any{
				"provider":      provider,
				"driver":        driver,
				"name":          connDisplayName(conn),
				"save-password": includePassword,
				"configuration": map[string]any{
					"host":              conn.Host,
					"port":              strconv.Itoa(conn.Port),
					"database":          conn.Database,
					"url":               jdbcBaseURL(conn),
					"configurationType": "MANUAL",
					"type":              "dev",
					"auth-model":        "native",
					"user":              conn.User,
				},
			},
		},
	}, "", "  ")
	dsJSON = append(dsJSON, '\n')

	credEntry := map[string]string{"user": conn.User}
	if includePassword {
		credEntry["password"] = conn.Password
	}
	credJSON, err := json.Marshal(map[string]any{
		connID: map[string]any{"#connection": credEntry},
	})
	if err != nil {
		return nil, err
	}
	encCreds, err := encryptDBeaverCredentials(string(credJSON))
	if err != nil {
		return nil, err
	}

	metaXML, err := marshalXML(xml.Header, dbeaverMetaArchive{
		Version: "1",
		Source:  dbeaverMetaSource{Time: "0", Address: "127.0.0.1", Host: "localhost"},
		Projects: []dbeaverMetaProject{{
			Name:        proj,
			Description: "",
			Resources: []dbeaverMetaResource{
				{Name: ".project"},
				{Name: ".dbeaver", Resources: []dbeaverMetaResource{
					{Name: "data-sources.json"},
					{Name: "credentials-config.json"},
				}},
			},
		}},
	})
	if err != nil {
		return nil, err
	}

	projectXML, err := marshalXML(xml.Header, dbeaverProjectDesc{
		Name:    proj,
		Natures: []string{"org.jkiss.dbeaver.DBeaverNature"},
	})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := "projects/" + proj
	entries := []struct {
		name string
		data []byte
		dir  bool
	}{
		{"meta.xml", metaXML, false},
		{prefix + "/.project", projectXML, false},
		{prefix + "/.dbeaver/", nil, true},
		{prefix + "/.dbeaver/data-sources.json", dsJSON, false},
		{prefix + "/.dbeaver/credentials-config.json", encCreds, false},
	}
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Store}
		if e.dir {
			hdr.ExternalAttrs = 0x10
		}
		w, _ := zw.CreateHeader(hdr)
		if len(e.data) > 0 {
			_, _ = w.Write(e.data)
		}
	}
	_ = zw.Close()
	return buf.Bytes(), nil
}

type clientFilePayload struct {
	defaultName string
	binary      bool
	data        []byte
	hint        string
	extraNote   string
	warnPW      bool
}

func generateClientFile(conn branchConnection, format string, includePassword bool) (clientFilePayload, error) {
	stem := fileStem(conn)
	name := connDisplayName(conn)

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "dbeaver":
		b, err := buildDBeaverDBP(conn, includePassword)
		if err != nil {
			return clientFilePayload{}, err
		}
		return clientFilePayload{
			defaultName: stem + ".dbp",
			binary:      true,
			data:        b,
			hint:        "Import via File -> Import -> DBeaver -> Project in DBeaver.",
			warnPW:      includePassword,
		}, nil

	case "chat2db":
		types := map[string]string{
			"mysql": "MYSQL", "postgres": "POSTGRESQL", "mariadb": "MARIADB",
			"clickhouse": "CLICKHOUSE", "sqlite": "SQLITE", "oracle": "ORACLE",
			"sqlserver": "SQLSERVER", "mongodb": "MONGODB",
		}
		t, ok := types[conn.Engine]
		if !ok {
			return clientFilePayload{}, fmt.Errorf("chat2db format does not support engine %q", conn.Engine)
		}
		b, err := json.MarshalIndent([]map[string]any{{
			"alias": name,
			"type":  t,
			"host":  conn.Host,
			"port":  strconv.Itoa(conn.Port),
			"url":   jdbcBaseURL(conn),
			"user":  conn.User,
			"ssh":   map[string]bool{"use": false},
		}}, "", "  ")
		if err != nil {
			return clientFilePayload{}, err
		}
		return clientFilePayload{
			defaultName: stem + "-chat2db.json",
			data:        append(b, '\n'),
			hint:        "Import via Import Connections -> Chat2DB in Chat2DB.",
		}, nil

	case "datagrip":
		drvs := map[string][2]string{
			"mysql":    {"mysql.8", "com.mysql.cj.jdbc.Driver"},
			"postgres": {"postgresql", "org.postgresql.Driver"},
			"mariadb":  {"mariadb", "org.mariadb.jdbc.Driver"},
		}
		drv, ok := drvs[conn.Engine]
		if !ok {
			return clientFilePayload{}, fmt.Errorf("datagrip format does not support engine %q", conn.Engine)
		}
		id := deterministicUUID(fmt.Sprintf("datagrip:%s:%s:%d:%s", name, conn.Host, conn.Port, conn.User))
		xmlData, err := marshalXML(xml.Header, dataGripProject{
			Version: "4",
			Component: dataGripComponent{
				Name:           "DataSourceManagerImpl",
				Format:         "xml",
				MultifileModel: "true",
				DataSource: dataGripDataSource{
					Source:      "LOCAL",
					Name:        name,
					UUID:        id,
					DriverRef:   drv[0],
					Synchronize: true,
					JDBCDriver:  drv[1],
					JDBCURL:     jdbcBaseURL(conn),
					UserName:    conn.User,
					WorkingDir:  "$ProjectFileDir$",
				},
			},
		})
		if err != nil {
			return clientFilePayload{}, err
		}
		return clientFilePayload{
			defaultName: "dataSources-" + stem + ".xml",
			data:        xmlData,
			hint:        "Save or merge into .idea/dataSources.xml in DataGrip / JetBrains.",
			extraNote:   "JDBC URL: " + buildJDBCURL(conn, includePassword),
			warnPW:      includePassword,
		}, nil

	case "navicat":
		types := map[string]string{"mysql": "MYSQL", "postgres": "POSTGRESQL", "mariadb": "MARIADB"}
		ct, ok := types[conn.Engine]
		if !ok {
			return clientFilePayload{}, fmt.Errorf("navicat format does not support engine %q", conn.Engine)
		}
		xmlData, err := marshalXML(xml.Header, navicatConnections{
			Ver: "1.5",
			Connection: navicatConnection{
				ConnectionName: name,
				ConnType:       ct,
				Host:           conn.Host,
				Port:           conn.Port,
				Database:       conn.Database,
				UserName:       conn.User,
				SSH:            "false",
				SavePassword:   "false",
			},
		})
		if err != nil {
			return clientFilePayload{}, err
		}
		return clientFilePayload{
			defaultName: stem + ".ncx",
			data:        xmlData,
			hint:        "Import via File -> Import Connections in Navicat.",
		}, nil

	case "pgadmin":
		if conn.Engine != "postgres" {
			return clientFilePayload{}, fmt.Errorf("pgadmin format only supports postgres (got %q)", conn.Engine)
		}
		maintDB := conn.Database
		if maintDB == "" {
			maintDB = "postgres"
		}
		b, err := json.MarshalIndent(map[string]any{
			"Servers": map[string]any{
				"1": map[string]any{
					"Name":          name,
					"Group":         "db-branch",
					"Host":          conn.Host,
					"Port":          conn.Port,
					"MaintenanceDB": maintDB,
					"Username":      conn.User,
					"SSLMode":       "prefer",
				},
			},
		}, "", "  ")
		if err != nil {
			return clientFilePayload{}, err
		}
		return clientFilePayload{
			defaultName: stem + "-pgadmin-servers.json",
			data:        append(b, '\n'),
			hint:        "Import via Tools -> Import/Export Servers in pgAdmin.",
		}, nil

	case "workbench":
		if conn.Engine != "mysql" && conn.Engine != "mariadb" {
			return clientFilePayload{}, fmt.Errorf("workbench format only supports mysql/mariadb (got %q)", conn.Engine)
		}
		id := deterministicUUID(fmt.Sprintf("workbench:%s:%s:%d:%s", name, conn.Host, conn.Port, conn.User))
		xmlData, err := marshalXML("<?xml version=\"1.0\"?>\n", workbenchData{
			GRTFormat: "2.0",
			List: workbenchList{
				Type:              "list",
				ContentType:       "object",
				ContentStructName: "db.mgmt.Connection",
				Connection: workbenchConnObj{
					Type:           "object",
					StructName:     "db.mgmt.Connection",
					ID:             id,
					StructChecksum: "0x96ba47d8",
					Items: []any{
						workbenchLink{Type: "object", StructName: "db.mgmt.Driver", Key: "driver", Text: "com.mysql.rdbms.mysql.driver.native"},
						workbenchValue{Type: "string", Key: "hostIdentifier", Text: "Mysql@" + formatHostPort(conn.Host, conn.Port)},
						workbenchValue{Type: "int", Key: "isDefault", Text: "0"},
						workbenchValue{Type: "dict", Key: "modules"},
						workbenchValue{
							Type: "dict",
							Key:  "parameterValues",
							Children: []workbenchValue{
								{Type: "string", Key: "defaultSchema", Text: conn.Database},
								{Type: "string", Key: "hostName", Text: conn.Host},
								{Type: "string", Key: "name", Text: name},
								{Type: "int", Key: "port", Text: strconv.Itoa(conn.Port)},
								{Type: "string", Key: "userName", Text: conn.User},
								{Type: "string", Key: "useSSL", Text: "IF_AVAILABLE"},
							},
						},
						workbenchLink{Type: "object", StructName: "db.mgmt.ServerInstance", Key: "serverInstance"},
					},
				},
			},
		})
		if err != nil {
			return clientFilePayload{}, err
		}
		return clientFilePayload{
			defaultName: stem + "-workbench-connections.xml",
			data:        xmlData,
			hint:        "Rename/merge into connections.xml in your MySQL Workbench config folder.",
		}, nil

	case "tableplus":
		return clientFilePayload{
			data:   []byte(buildTablePlusURL(conn, includePassword) + "\n"),
			hint:   "Import via Import Connection -> From URL in TablePlus.",
			warnPW: includePassword,
		}, nil

	default:
		return clientFilePayload{}, fmt.Errorf("unknown --format %q (supported: dbeaver, chat2db, datagrip, navicat, pgadmin, workbench, tableplus)", format)
	}
}

func (a *App) writeClientFile(conn branchConnection, format, outPath string, includePassword, force bool) error {
	p, err := generateClientFile(conn, format, includePassword)
	if err != nil {
		return err
	}
	if p.warnPW {
		fmt.Fprintln(a.Stderr, "Warning: output contains a password (plaintext or fixed DBeaver local key); do not share or commit it.")
	}
	if p.defaultName == "" && outPath == "" {
		outPath = "-"
	}
	if outPath == "" {
		outPath = p.defaultName
	}
	if outPath == "-" {
		if p.binary {
			return errors.New("binary .dbp archive cannot be written to stdout; specify --out <file.dbp> or omit --out")
		}
		if _, err := a.Stdout.Write(p.data); err != nil {
			return err
		}
		if p.extraNote != "" {
			fmt.Fprintln(a.Stderr, p.extraNote)
		}
		return nil
	}
	if !force {
		f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("file %q already exists (pass --force to overwrite)", outPath)
			}
			return err
		}
		_, werr := f.Write(p.data)
		if cerr := f.Close(); werr != nil || cerr != nil {
			return errors.Join(werr, cerr)
		}
	} else {
		if err := os.WriteFile(outPath, p.data, 0o600); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.Stderr, "Wrote %s\n%s\n", outPath, p.hint)
	if p.extraNote != "" {
		fmt.Fprintln(a.Stderr, p.extraNote)
	}
	return nil
}
