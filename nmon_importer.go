// nmon2influxdb
// import nmon report in InfluxDB
// author: adejoux@djouxtech.net

package main

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/adejoux/influxdbclient"
	"github.com/codegangsta/cli"
)

var hostRegexp = regexp.MustCompile(`^AAA,host,(\S+)`)
var osRegexp = regexp.MustCompile(`^AAA,.*(Linux|AIX)`)
var timeRegexp = regexp.MustCompile(`^ZZZZ,([^,]+),(.*)$`)
var intervalRegexp = regexp.MustCompile(`^AAA,interval,(\d+)`)
var headerRegexp = regexp.MustCompile(`^AAA|^BBB|^UARG|,T\d`)
var infoRegexp = regexp.MustCompile(`AAA,(.*)`)
var badRegexp = regexp.MustCompile(`,,`)
var cpuallRegexp = regexp.MustCompile(`^CPU\d+|^SCPU\d+|^PCPU\d+`)
var diskallRegexp = regexp.MustCompile(`^DISK`)
var skipRegexp = regexp.MustCompile(`T0+,|^Z|^TOP,%CPU`)
var statsRegexp = regexp.MustCompile(`^[^,]+?,(T\d+)`)
var topRegexp = regexp.MustCompile(`^TOP,\d+,(T\d+)`)
var nfsRegexp = regexp.MustCompile(`^NFS`)
var nameRegexp = regexp.MustCompile(`(\d+)$`)

//NmonImport is the entry point for subcommand nmon import
func NmonImport(c *cli.Context) {

	if len(c.Args()) < 1 {
		fmt.Printf("file name or directory needs to be provided\n")
		os.Exit(1)
	}

	// parsing parameters
	config := ParseParameters(c)

	//getting databases connections
	influxdb := config.GetDataDB()
	influxdbLog := config.GetLogDB()

	nmonFiles := new(NmonFiles)
	nmonFiles.Parse(c.Args(), config.ImportSSHUser, config.ImportSSHKey)

	for _, nmonFile := range nmonFiles.Valid() {
		var count int64
		count = 0
		nmon := InitNmon(config, nmonFile)

		lines := nmonFile.Content()

		var userSkipRegexp *regexp.Regexp

		if len(config.ImportSkipMetrics) > 0 {
			skipped := strings.Replace(config.ImportSkipMetrics, ",", "|", -1)
			userSkipRegexp = regexp.MustCompile(skipped)
		}

		var last string
		filters := new(influxdbclient.Filters)
		filters.Add("file", path.Base(nmonFile.Name), "text")

		result, err := influxdbLog.ReadLastPoint("value", filters, "timestamp")
		check(err)

		var lastTime time.Time
		if !nmon.Config.ImportForce && len(result) > 0 {
			lastTime, err = nmon.ConvertTimeStamp(result[1].(string))
		} else {
			lastTime, err = nmon.ConvertTimeStamp("00:00:00,01-JAN-1900")
		}
		check(err)

		origChecksum, err := influxdbLog.ReadLastPoint("value", filters, "checksum")
		check(err)

		ckfield := map[string]interface{}{"value": nmonFile.Checksum()}
		if !nmon.Config.ImportForce && len(origChecksum) > 0 {

			if origChecksum[1].(string) == nmonFile.Checksum() {
				fmt.Printf("file not changed since last import: %s\n", nmonFile.Name)
				continue
			}
		}
		for _, line := range lines {

			if cpuallRegexp.MatchString(line) && !config.ImportAllCpus {
				continue
			}

			if diskallRegexp.MatchString(line) && config.ImportSkipDisks {
				continue
			}

			if skipRegexp.MatchString(line) {
				continue
			}

			if statsRegexp.MatchString(line) {
				matched := statsRegexp.FindStringSubmatch(line)
				elems := strings.Split(line, ",")
				name := elems[0]

				if len(config.ImportSkipMetrics) > 0 {
					if userSkipRegexp.MatchString(name) {
						if nmon.Debug {
							fmt.Printf("metric skipped : %s\n", name)
						}
						continue
					}
				}

				timeStr, getErr := nmon.GetTimeStamp(matched[1])
				check(getErr)
				last = timeStr
				timestamp, convErr := nmon.ConvertTimeStamp(timeStr)
				check(convErr)
				if timestamp.Before(lastTime) && !nmon.Config.ImportForce {
					continue
				}

				for i, value := range elems[2:] {
					if len(nmon.DataSeries[name].Columns) < i+1 {
						if nmon.Debug {
							fmt.Printf(line)
							fmt.Printf("Entry added position %d in serie %s since nmon start: skipped\n", i+1, name)
						}
						continue
					}
					column := nmon.DataSeries[name].Columns[i]
					tags := map[string]string{"host": nmon.Hostname, "name": column}

					// try to convert string to integer
					converted, parseErr := strconv.ParseFloat(value, 64)
					if parseErr != nil {
						//if not working, skip to next value. We don't want text values in InfluxDB.
						continue
					}

					//send integer if it worked
					field := map[string]interface{}{"value": converted}

					measurement := ""
					if nfsRegexp.MatchString(name) || cpuallRegexp.MatchString(name) {
						measurement = name
					} else {
						measurement = nameRegexp.ReplaceAllString(name, "")
					}

					influxdb.AddPoint(measurement, timestamp, field, tags)

					if influxdb.PointsCount() == 10000 {
						err = influxdb.WritePoints()
						check(err)
						count += influxdb.PointsCount()
						influxdb.ClearPoints()
						fmt.Printf("#")
					}
				}
			}

			if topRegexp.MatchString(line) {
				matched := topRegexp.FindStringSubmatch(line)
				elems := strings.Split(line, ",")
				name := elems[0]
				if len(config.ImportSkipMetrics) > 0 {
					if userSkipRegexp.MatchString(name) {
						if nmon.Debug {
							fmt.Printf("metric skipped : %s\n", name)
						}
						continue
					}
				}

				timeStr, getErr := nmon.GetTimeStamp(matched[1])
				check(getErr)
				timestamp, convErr := nmon.ConvertTimeStamp(timeStr)
				check(convErr)

				if len(elems) < 14 {
					fmt.Printf("error TOP import:")
					fmt.Println(elems)
					continue
				}

				for i, value := range elems[3:12] {
					column := nmon.DataSeries["TOP"].Columns[i]

					var wlmclass string
					if len(elems) < 15 {
						wlmclass = "none"
					} else {
						wlmclass = elems[14]
					}

					tags := map[string]string{"host": nmon.Hostname, "name": column, "pid": elems[1], "command": elems[13], "wlm": wlmclass}

					// try to convert string to integer
					converted, parseErr := strconv.ParseFloat(value, 64)
					if parseErr != nil {
						//if not working, skip to next value. We don't want text values in InfluxDB.
						continue
					}

					//send integer if it worked
					field := map[string]interface{}{"value": converted}

					influxdb.AddPoint("TOP", timestamp, field, tags)

					if influxdb.PointsCount() == 10000 {
						err = influxdb.WritePoints()
						check(err)
						count += influxdb.PointsCount()
						influxdb.ClearPoints()
						fmt.Printf("#")
					}
				}
			}
		}
		// flushing remaining data
		influxdb.WritePoints()
		count += influxdb.PointsCount()
		fmt.Printf("\nFile %s imported : %d points !\n", nmonFile.Name, count)
		if config.ImportBuildDashboard {
			NmonDashboardFile(config, nmonFile.Name)
		}

		if len(last) > 0 {
			field := map[string]interface{}{"value": last}
			tag := map[string]string{"file": path.Base(nmonFile.Name)}
			lasttime, _ := nmon.ConvertTimeStamp("now")
			influxdbLog.AddPoint("timestamp", lasttime, field, tag)
			influxdbLog.AddPoint("checksum", lasttime, ckfield, tag)
			err = influxdbLog.WritePoints()
			check(err)
		}
	}
}
