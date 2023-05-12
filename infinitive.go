package main

	// Ref: https://github.com/acd/infinitive
	// Ref: https://github.com/elazarl/go-bindata-assetfs
	// Installed to build assets
	//		go get github.com/go-bindata/go-bindata/...
	//		go get github.com/elazarl/go-bindata-assetfs/...
	// Help Ref: https://github.com/inconshreveable/ngrok/issues/181

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"
	"strconv"
	"bufio"
	"github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"
	"io"
	"io/ioutil"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/go-echarts/go-echarts/v2/types"
)

type TStatZoneConfig struct {
	CurrentTemp     uint8  `json:"currentTemp"`
	CurrentHumidity uint8  `json:"currentHumidity"`
	OutdoorTemp     uint8  `json:"outdoorTemp"`
	Mode            string `json:"mode"`
	Stage           uint8  `json:"stage"`
	FanMode         string `json:"fanMode"`
	Hold            *bool  `json:"hold"`
	HeatSetpoint    uint8  `json:"heatSetpoint"`
	CoolSetpoint    uint8  `json:"coolSetpoint"`
	RawMode         uint8  `json:"rawMode"`
}

type AirHandler struct {
	BlowerRPM  uint16 `json:"blowerRPM"`
	AirFlowCFM uint16 `json:"airFlowCFM"`
	ElecHeat   bool   `json:"elecHeat"`
}

type HeatPump struct {
	CoilTemp    float32 `json:"coilTemp"`
	OutsideTemp float32 `json:"outsideTemp"`
	Stage       uint8   `json:"stage"`
}

var infinity *InfinityProtocol

// String candidates for redefinition on build using -ldflags
var	Version			= "development"
var	filePath		= "/var/lib/infinitive/"
var	fileName		= "Infinitive.csv"
var	logPath			= "/var/log/infinitive/"
var TemperatureSuffix = "_Temperature.html"


// aded: Global defs to support periodic write to file
var fileHvacHistory *os.File
var blowerRPM       uint16
var	currentTemp     uint8
var	outdoorTemp     uint8
var	heatSet			uint8
var	coolSet			uint8
var	hvacMode		string
var outTemp			int
var	inTemp			int
var	fanRPM			int
var	index			int
var	htmlChartTable	string


// Original Infinitive code with minor changes...
func getConfig() (*TStatZoneConfig, bool) {
	cfg := TStatZoneParams{}
	ok := infinity.ReadTable(devTSTAT, &cfg)
	if !ok {
		return nil, false
	}

	params := TStatCurrentParams{}
	ok = infinity.ReadTable(devTSTAT, &params)
	if !ok {
		return nil, false
	}

	hold := new(bool)
	*hold = cfg.ZoneHold&0x01 == 1

	// Save for periodic cron1 to pick
	currentTemp	= params.Z1CurrentTemp
	inTemp		= int(currentTemp)
	outdoorTemp	= params.OutdoorAirTemp
	outTemp		= int(outdoorTemp)
	heatSet		= cfg.Z1HeatSetpoint
	coolSet		= cfg.Z1CoolSetpoint
	hvacMode	= rawModeToString(params.Mode & 0xf)

	return &TStatZoneConfig{
		CurrentTemp:     params.Z1CurrentTemp,
		CurrentHumidity: params.Z1CurrentHumidity,
		OutdoorTemp:     params.OutdoorAirTemp,
		Mode:            rawModeToString(params.Mode & 0xf),
		Stage:           params.Mode >> 5,
		FanMode:         rawFanModeToString(cfg.Z1FanMode),
		Hold:            hold,
		HeatSetpoint:    cfg.Z1HeatSetpoint,
		CoolSetpoint:    cfg.Z1CoolSetpoint,
		RawMode:         params.Mode,
	}, true
}

func getTstatSettings() (*TStatSettings, bool) {
	tss := TStatSettings{}
	ok := infinity.ReadTable(devTSTAT, &tss)
	if !ok {
		return nil, false
	}

	return &TStatSettings{
		BacklightSetting: tss.BacklightSetting,
		AutoMode:         tss.AutoMode,
		DeadBand:         tss.DeadBand,
		CyclesPerHour:    tss.CyclesPerHour,
		SchedulePeriods:  tss.SchedulePeriods,
		ProgramsEnabled:  tss.ProgramsEnabled,
		TempUnits:        tss.TempUnits,
		DealerName:       tss.DealerName,
		DealerPhone:      tss.DealerPhone,
	}, true
}

func getAirHandler() (AirHandler, bool) {
	b := cache.get("blower")
	tb, ok := b.(*AirHandler)
	if !ok {
		return AirHandler{}, false
	}
	return *tb, true
}

func getHeatPump() (HeatPump, bool) {
	h := cache.get("heatpump")
	th, ok := h.(*HeatPump)
	if !ok {
		return HeatPump{}, false
	}
	return *th, true
}

func statePoller() {
	for {
		c, ok := getConfig()
		if ok {
			cache.update("tstat", c)
		}

		time.Sleep(time.Second * 1)
	}
}

func attachSnoops() {
	// Snoop Heat Pump responses
	infinity.snoopResponse(0x5000, 0x51ff, func(frame *InfinityFrame) {
		data := frame.data[3:]
		heatPump, ok := getHeatPump()
		if ok {
			if bytes.Equal(frame.data[0:3], []byte{0x00, 0x3e, 0x01}) {
				heatPump.CoilTemp = float32(binary.BigEndian.Uint16(data[2:4])) / float32(16)
				heatPump.OutsideTemp = float32(binary.BigEndian.Uint16(data[0:2])) / float32(16)
				log.Debugf("heat pump coil temp is: %f", heatPump.CoilTemp)
				log.Debugf("heat pump outside temp is: %f", heatPump.OutsideTemp)
				cache.update("heatpump", &heatPump)
			} else if bytes.Equal(frame.data[0:3], []byte{0x00, 0x3e, 0x02}) {
				heatPump.Stage = data[0] >> 1
				log.Debugf("HP stage is: %d", heatPump.Stage)
				cache.update("heatpump", &heatPump)
			}
		}
	})

	// Snoop Air Handler responses
	infinity.snoopResponse(0x4000, 0x42ff, func(frame *InfinityFrame) {
		data := frame.data[3:]
		airHandler, ok := getAirHandler()
		if ok {
			if bytes.Equal(frame.data[0:3], []byte{0x00, 0x03, 0x06}) {
				airHandler.BlowerRPM = binary.BigEndian.Uint16(data[1:5])
				log.Debugf("blower RPM is: %d", airHandler.BlowerRPM)
				cache.update("blower", &airHandler)
				blowerRPM = airHandler.BlowerRPM		// added
				fanRPM = int(blowerRPM)					// added
			} else if bytes.Equal(frame.data[0:3], []byte{0x00, 0x03, 0x16}) {
				airHandler.AirFlowCFM = binary.BigEndian.Uint16(data[4:8])
				airHandler.ElecHeat = data[0]&0x03 != 0
				log.Debugf("air flow CFM is: %d", airHandler.AirFlowCFM)
				cache.update("blower", &airHandler)
			}
		}
	})

}

// Function to find HTML files and prepare table of links, bool argument controls table only or full html page
func makeTableHTMLfiles( tableOnly bool ) {
	// Identify the html files, produce 2 column html table of links
	htmlLinks, err := os.OpenFile(filePath+"htmlLinks.html", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
			log.Error("makeTableHTMLfiles:htmlFile Create Failure.")
	}
	if !tableOnly {
		timeStr := time.Now().Format("2006-01-02 15:04:05")
		htmlLinks.WriteString( "<!-- infinitive.makeTableHTMLfiles(): " + timeStr + " -->\n" )
		htmlLinks.WriteString( "<!DOCTYPE html>\n<html>\n<head>\n<title>HVAC Saved Measuremnts " + timeStr + "</title>\n</head>\n" )
		htmlLinks.WriteString( "<body>\n<h2>HVAC Saved Measuremnts " + timeStr + "</h2>\n" )
	}
	htmlLinks.WriteString( "<table width=\"600\" border=\"1\">\n" )
	files, err := ioutil.ReadDir( filePath[0:len(filePath)-1] )  // does not want trailing /
	if err != nil {
		log.Error("makeTableHTMLfiles() - file dirctory read error.")
		log.Error(err)
	} else {
		index = 0
		for _, file := range files {
			fileName := file.Name()
			length := len(fileName)
			// Only process temprature html files...
			if fileName[length-1] == 'l' && fileName[0]!='h' {
				// make two column table...
				if index % 2 == 0 {
					htmlLinks.WriteString( "  <tr>\n" )
				}
				htmlLinks.WriteString( "    <td><a href=\"" + filePath + fileName + "\" target=\"_blank\" rel=\"noopener noreferrer\">" + fileName + "</a></td>\n" )
					if index % 2 != 0 {
						htmlLinks.WriteString( " </tr>\n" )
				}
				index++
			}
		}
		if index % 2 == 1 {
			htmlLinks.WriteString( " </tr>\n" )
		}
	}
	htmlLinks.WriteString( "</table>\n" )
	if !tableOnly {
		htmlLinks.WriteString( "</body>\n</html>\n\n" )
	}
	htmlLinks.Close()
	return
}

func main() {
	var HeaderString	= "Date,Time,FracTime,Heat Set,Cool Set,Outdoor Temp,Current Temp,blowerRPM\n"
	var dailyFileName, s2	string
	var f64					float64

	httpPort := flag.Int("httpport", 8080, "HTTP port to listen on")
	serialPort := flag.String("serial", "", "path to serial port")

	flag.Parse()

	if len(*serialPort) == 0 {
		log.Info("must provide serial\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	log.SetLevel(log.ErrorLevel)		// was log.DebugLevel

	infinity = &InfinityProtocol{device: *serialPort}
	airHandler := new(AirHandler)
	heatPump   := new(HeatPump)
	cache.update("blower", airHandler)
	cache.update("heatpump", heatPump)
	attachSnoops()
	err := infinity.Open()
	if err != nil {
		log.Panicf("error opening serial port: %s", err.Error())
	}

	// added for data collection and charting
	dayf	:= make( [] float32, 2000 )
	inTmp	:= make( [] int,	 2000 )
	outTmp	:= make( [] int,	 2000 )
	motRPM	:= make( [] int,	 2000 )
	dt := time.Now()
	//	Save the data in a file, observed crashing requires charting from file
	fileHvacHistory, err = os.OpenFile(filePath+fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0664)
	if err != nil {
			log.Error("Infinitive Data File Open Failure.")
	}
	fileHvacHistory.WriteString( HeaderString )

	// References for periodic execution:
	//		https://pkg.go.dev/github.com/robfig/cron?utm_source=godoc
	//		https://github.com/robfig/cron
	// cron Job 1 - every 4 minutes - collect to Infinitive.csv
	// cron Job 2 - after last data of the day - close, rename, open new Infinitive.csv, & produce html from last file
	// cron Job 3 - purge daily files after 14 days
	// cron job 4 - delete log files 2x per month, 1st & 15th
	// Set up cron 1 for 4 minute data collection
	cronJob1 := cron.New(cron.WithSeconds())
	cronJob1.AddFunc("0 */4 * * * *", func () {
		dt := time.Now()
		frcDay :=  float32(dt.Day()) + 4.16667*(float32(dt.Hour()) + float32(dt.Minute())/60.0)/100.0
		s1 := fmt.Sprintf( "%s,%09.4f,%04d,%04d,%04d,%04d,%04d,%s\n", dt.Format("2006-01-02T15:04:05"),
							frcDay,heatSet, coolSet, outdoorTemp, currentTemp, blowerRPM, hvacMode )
		fileHvacHistory.WriteString(s1)
	} )
	cronJob1.Start()

	// Set up cron 2 for daily file save after last collection, data clean up, and charting
	cronJob2 := cron.New(cron.WithSeconds())
	cronJob2.AddFunc( "2 59 23 * * *", func() {
		// Close, rename, open new Infinitive.csv
		err = fileHvacHistory.Close()
		if err != nil {
			log.Error("infinitive.go cron 2 - error closing:" + filePath+fileName)
			os.Exit(0)
		}
		dailyFileName = fmt.Sprintf( "%s%4d-%02d-%02d_%s", filePath, dt.Year(), dt.Month(), dt.Day(), fileName)
		log.Info("infinitive.go cron 2 - daily filename: " + dailyFileName)
		err = os.Rename(filePath+fileName,dailyFileName)
		if err != nil {
			log.Error("infinitive.go cron 2 - unable to rename old "+filePath+fileName+" to "+dailyFileName)
			os.Exit(0)
		}
		// Reopen/Open new Infinitive.csv
		fileHvacHistory, err = os.OpenFile(filePath+fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0664)
		if err != nil {
			log.Error("infinitive.go cron Job 2, error on reopen of:"+filePath+fileName)
			os.Exit(0)
		}
		fileHvacHistory.WriteString( HeaderString )
		// Open the renamed file to read captured data
		fileDaily, err := os.OpenFile( dailyFileName, os.O_RDONLY, 0 )
		if err != nil {
			log.Error("infinitive.go cron 2 - unable to open daily file for read: "+dailyFileName)
			os.Exit(0)
		}
		// Read and prepare days data for charting
		items1 := make( []opts.LineData, 0 )
		items2 := make( []opts.LineData, 0 )
		items3 := make( []opts.LineData, 0 )
		index = 0
		filescan := bufio.NewScanner( fileDaily )
		for filescan.Scan() {
			s2 = filescan.Text()
			if filescan.Err() != nil {
				log.Error("infinitive.go cron 2. filesscan read error:" + s2)
			}
			if s2[0] != 'D' {		// Header lines start with D, skip'em
				f64, err	= strconv.ParseFloat( s2[20:29], 32 )
				dayf[index]	= float32(f64)
				// Extract and save the indoor and outdoor temps in the slices (not yet used)
				outTmp[index], err	= strconv.Atoi( s2[40:44] )
				if outTmp[index]==0 || outTmp[index]>130 {	// outTmp could be zero, but could be error
					outTmp[index] = outTmp[index-1]			// worry about index==0 later
				}
				inTmp[index], err	= strconv.Atoi( s2[45:49] )
				if inTmp[index]==0 || inTmp[index]>110 {
					inTmp[index] = inTmp[index-1]			// worry about index==0 later
				}
				motRPM[index], err = strconv.Atoi( s2[50:54] )
				// Set low-med-Hi ranges to later improve chart, for now %range matches temp# range
				if motRPM[index] < 200 {
					motRPM[index] = 0
				} else if motRPM[index] < 550 {
					motRPM[index] = 34
				} else if motRPM[index] < 750 {
					motRPM[index] = 66
				} else {
					motRPM[index] = 100
				}
				items1 = append( items1, opts.LineData{ Value: inTmp[index]  } )
				items2 = append( items2, opts.LineData{ Value: outTmp[index] } )
				items3 = append( items3, opts.LineData{ Value: motRPM[index] } )
				index++
			}
		}
		fileDaily.Close()
		// echarts referenece: https://github.com/go-echarts/go-echarts
		Line := charts.NewLine()
		Line.SetGlobalOptions(
			charts.WithInitializationOpts(opts.Initialization{Theme: types.ThemeWesteros}),
			charts.WithTitleOpts(opts.Title{
				Title:    "Infinitive " + Version + " HVAC Daily Chart",
				Subtitle: "Indoor and Outdoor Temperatues from " + dailyFileName,
			} ) )
		// Chart the Indoor and Outdoor temps (to start). How to use date/time string as time?
		Line.SetXAxis( dayf[0:index-1])
		Line.AddSeries("Indoor Temp", 	items1[0:index-1])
		Line.AddSeries("Outdoor Temp",	items2[0:index-1])
		Line.SetSeriesOptions(charts.WithMarkLineNameTypeItemOpts(opts.MarkLineNameTypeItem{Name: "Minimum", Type: "min"}))
		Line.SetSeriesOptions(charts.WithMarkLineNameTypeItemOpts(opts.MarkLineNameTypeItem{Name: "Maximum", Type: "max"}))
		Line.AddSeries("Fan RPM%",		items3[0:index-1])
		Line.SetSeriesOptions(charts.WithLineChartOpts(opts.LineChart{Smooth: true}))
		fileStr := fmt.Sprintf("%s%04d-%02d-%02d%s", filePath, dt.Year(), dt.Month(), dt.Day(), TemperatureSuffix)
		fHTML, err := os.OpenFile( fileStr, os.O_CREATE|os.O_RDWR, 0664 )
		if err == nil {
			// Example Ref: https://github.com/go-echarts/examples/blob/master/examples/boxplot.go
			Line.Render(io.MultiWriter(fHTML))
		} else {
			log.Error("Infinitive.go cron 2, error creating html rendered file.")
		}
		fHTML.Close()
	} )
	cronJob2.Start()

	// Set up cron 3 to purge old daily data files, identify all html files
	cronJob3 := cron.New(cron.WithSeconds())
	cronJob3.AddFunc( "3 5 0 * * *", func () {
		// purge old csv files
		shellString := "sudo find " + filePath + "*.csv -type f -mtime +14 -delete;"
		log.Error("infinitive.go cron 3, issue old file purge shell command: " + shellString)
		shellCmd := exec.Command(shellString)
		err := shellCmd.Run()
		if err != nil {
			log.Error("Infinitve.go cron 3, csv purge script Failed." + shellString)
		}
		// purge old html files
		shellString = "sudo find " + filePath + "*.html -type f -mtime +14 -delete;"
		log.Error("infinitive.go cron 3, issue old file purge shell command: " + shellString)
		shellCmd = exec.Command(shellString)
		err = shellCmd.Run()
		if err != nil {
			log.Error("Infinitve.go cron 3, html file purge script Failed." + shellString)
		}
		makeTableHTMLfiles( false )
	} )
	cronJob3.Start()

	// Set up cron 4 to delete log files 2x per month
	cronJob4 := cron.New(cron.WithSeconds())
	cronJob4.AddFunc( "4 0 1 1,15 * *", func () {
		// remove log files least they grow unbounded
		shellString := "sudo rm " + logPath + "*.log"
		log.Error("infinitive.go cron 4 issue remove log files command: " + shellString)
		shellCmd := exec.Command(shellString)
		err := shellCmd.Run()
		if err != nil {
			log.Error("Infinitve.go cron 4, log file purge error: " + shellString)
		}
	} )
	cronJob4.Start()



	go statePoller()
	webserver(*httpPort)
}
