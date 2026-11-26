package cli

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	words := [4]uint32{0x811c9dc5, 0x01000193, 0x9e3779b9, 0x85ebca6b}
	for i := 0; i < len(seed); i++ {
		c := uint32(seed[i])
		for j := range words {
			words[j] = (words[j] ^ (c + uint32(j)*31)) * 0x01000193
		}
	}
	h := fmt.Sprintf("%08x%08x%08x%08x", words[0], words[1], words[2], words[3])
	return fmt.Sprintf("%s-%s-4%s-8%s-%s", h[0:8], h[8:12], h[13:16], h[17:20], h[20:32])
}

var xmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func xmlEscape(s string) string { return xmlReplacer.Replace(s) }

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

func encryptDBeaverCredentials(plaintextJSON string) []byte {
	raw := []byte(plaintextJSON)
	pad := aes.BlockSize - (len(raw) % aes.BlockSize)
	padded := make([]byte, len(raw)+pad)
	copy(padded, raw)
	for i := len(raw); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	iv := make([]byte, aes.BlockSize)
	_, _ = rand.Read(iv)
	blk, _ := aes.NewCipher(dbeaverLocalKey)
	out := make([]byte, aes.BlockSize+len(padded))
	copy(out[:aes.BlockSize], iv)
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(out[aes.BlockSize:], padded)
	return out
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
	credJSON, _ := json.Marshal(map[string]any{
		connID: map[string]any{"#connection": credEntry},
	})
	encCreds := encryptDBeaverCredentials(string(credJSON))

	metaXML := strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<archive version="1">`,
		`  <source time="0" address="127.0.0.1" host="localhost"/>`,
		`  <libraries/>`,
		`  <projects>`,
		fmt.Sprintf(`    <project name="%s" description="">`, xmlEscape(proj)),
		`      <resource name=".project"/>`,
		`      <resource name=".dbeaver">`,
		`        <resource name="data-sources.json"/>`,
		`        <resource name="credentials-config.json"/>`,
		`      </resource>`,
		`    </project>`,
		`  </projects>`,
		`</archive>`,
		"",
	}, "\n")

	projectXML := strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<projectDescription>`,
		fmt.Sprintf(`  <name>%s</name>`, xmlEscape(proj)),
		`  <comment></comment>`,
		`  <projects/>`,
		`  <buildSpec/>`,
		`  <natures>`,
		`    <nature>org.jkiss.dbeaver.DBeaverNature</nature>`,
		`  </natures>`,
		`</projectDescription>`,
		"",
	}, "\n")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := "projects/" + proj
	entries := []struct {
		name string
		data []byte
		dir  bool
	}{
		{"meta.xml", []byte(metaXML), false},
		{prefix + "/.project", []byte(projectXML), false},
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
		uuid := deterministicUUID(fmt.Sprintf("datagrip:%s:%s:%d:%s", name, conn.Host, conn.Port, conn.User))
		xml := strings.Join([]string{
			`<?xml version="1.0" encoding="UTF-8"?>`,
			`<project version="4">`,
			`  <component name="DataSourceManagerImpl" format="xml" multifile-model="true">`,
			fmt.Sprintf(`    <data-source source="LOCAL" name="%s" uuid="%s">`, xmlEscape(name), uuid),
			fmt.Sprintf(`      <driver-ref>%s</driver-ref>`, drv[0]),
			`      <synchronize>true</synchronize>`,
			fmt.Sprintf(`      <jdbc-driver>%s</jdbc-driver>`, drv[1]),
			fmt.Sprintf(`      <jdbc-url>%s</jdbc-url>`, xmlEscape(jdbcBaseURL(conn))),
			fmt.Sprintf(`      <user-name>%s</user-name>`, xmlEscape(conn.User)),
			`      <working-dir>$ProjectFileDir$</working-dir>`,
			`    </data-source>`,
			`  </component>`,
			`</project>`,
			"",
		}, "\n")
		return clientFilePayload{
			defaultName: "dataSources-" + stem + ".xml",
			data:        []byte(xml),
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
		xml := strings.Join([]string{
			`<?xml version="1.0" encoding="UTF-8"?>`,
			`<Connections Ver="1.5">`,
			fmt.Sprintf(`  <Connection ConnectionName="%s" ConnType="%s" Host="%s" Port="%d" Database="%s" UserName="%s" SSH="false" SavePassword="false"/>`,
				xmlEscape(name), ct, xmlEscape(conn.Host), conn.Port, xmlEscape(conn.Database), xmlEscape(conn.User)),
			`</Connections>`,
			"",
		}, "\n")
		return clientFilePayload{
			defaultName: stem + ".ncx",
			data:        []byte(xml),
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
		xml := strings.Join([]string{
			`<?xml version="1.0"?>`,
			`<data grt_format="2.0">`,
			`  <value type="list" content-type="object" content-struct-name="db.mgmt.Connection">`,
			fmt.Sprintf(`    <value type="object" struct-name="db.mgmt.Connection" id="%s" struct-checksum="0x96ba47d8">`, id),
			`      <link type="object" struct-name="db.mgmt.Driver" key="driver">com.mysql.rdbms.mysql.driver.native</link>`,
			fmt.Sprintf(`      <value type="string" key="hostIdentifier">%s</value>`, xmlEscape("Mysql@"+formatHostPort(conn.Host, conn.Port))),
			`      <value type="int" key="isDefault">0</value>`,
			`      <value type="dict" key="modules"/>`,
			`      <value type="dict" key="parameterValues">`,
			fmt.Sprintf(`        <value type="string" key="defaultSchema">%s</value>`, xmlEscape(conn.Database)),
			fmt.Sprintf(`        <value type="string" key="hostName">%s</value>`, xmlEscape(conn.Host)),
			fmt.Sprintf(`        <value type="string" key="name">%s</value>`, xmlEscape(name)),
			fmt.Sprintf(`        <value type="int" key="port">%d</value>`, conn.Port),
			fmt.Sprintf(`        <value type="string" key="userName">%s</value>`, xmlEscape(conn.User)),
			`        <value type="string" key="useSSL">IF_AVAILABLE</value>`,
			`      </value>`,
			`      <link type="object" struct-name="db.mgmt.ServerInstance" key="serverInstance"/>`,
			`    </value>`,
			`  </value>`,
			`</data>`,
			"",
		}, "\n")
		return clientFilePayload{
			defaultName: stem + "-workbench-connections.xml",
			data:        []byte(xml),
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
